package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *postgresStore) IssueBrowserSession(ctx context.Context, actor accesscontrol.Actor, authMethod string, expiresAt time.Time) (BrowserSessionCredential, error) {
	if !validBrowserSessionActor(actor) || !validBrowserAuthMethod(authMethod) || !expiresAt.After(time.Now()) {
		return BrowserSessionCredential{}, accesscontrol.ErrAuthenticationRequired
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return BrowserSessionCredential{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	credential, err := issueBrowserSessionInTransaction(ctx, tx, actor, authMethod, expiresAt)
	if err != nil {
		return BrowserSessionCredential{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BrowserSessionCredential{}, fmt.Errorf("commit browser session: %w", err)
	}
	return credential, nil
}

func issueBrowserSessionInTransaction(
	ctx context.Context,
	tx pgx.Tx,
	actor accesscontrol.Actor,
	authMethod string,
	expiresAt time.Time,
) (BrowserSessionCredential, error) {
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return BrowserSessionCredential{}, err
	}
	if err := lockBrowserSessionSource(ctx, tx, actor); err != nil {
		return BrowserSessionCredential{}, err
	}
	rawKey, err := generateControlCredential()
	if err != nil {
		return BrowserSessionCredential{}, err
	}
	credential, err := insertUserControlCredential(ctx, tx, IssueCredentialInput{
		// A user can sign in from multiple browsers. The secret-safe prefix keeps
		// active names distinct without exposing the credential or blocking re-login.
		UserID: actor.SubjectID, Name: "Browser session " + accesscontrol.CredentialPrefix(rawKey), ExpiresAt: &expiresAt,
		Source: browserSessionSource(authMethod), AuthMethod: authMethod,
	}, rawKey)
	if err != nil {
		return BrowserSessionCredential{}, err
	}
	revision, err := finalizeBrowserSessionMutation(ctx, tx, actor, credential.ID, "user.browser_session.create", authMethod, true)
	if err != nil {
		return BrowserSessionCredential{}, err
	}
	return BrowserSessionCredential{
		SubjectID: actor.SubjectID, CredentialID: credential.ID, RawKey: rawKey,
		ExpiresAt: expiresAt, AuthorizationRevision: revision,
	}, nil
}

func (s *postgresStore) RevokeBrowserSession(ctx context.Context, actor accesscontrol.Actor, at time.Time) (BrowserLogoutContext, error) {
	if !validBrowserSessionActor(actor) {
		return BrowserLogoutContext{}, accesscontrol.ErrAuthenticationRequired
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return BrowserLogoutContext{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return BrowserLogoutContext{}, err
	}
	logout, err := lockBrowserLogoutContext(ctx, tx, actor, at)
	if err != nil {
		return BrowserLogoutContext{}, err
	}
	command, err := tx.Exec(ctx, `
		UPDATE fused_control_credentials
		SET revoked_at = $3
		WHERE id = $1 AND subject_id = $2 AND revoked_at IS NULL AND expires_at IS NOT NULL
			AND source IN ('managed_login', 'license_exchange', 'api_key_exchange')
	`, actor.CredentialID, actor.SubjectID, at)
	if err != nil {
		return BrowserLogoutContext{}, err
	}
	if command.RowsAffected() != 1 {
		return BrowserLogoutContext{}, accesscontrol.ErrAuthenticationRequired
	}
	if _, err := tx.Exec(ctx, `DELETE FROM fused_browser_logout_contexts WHERE credential_id = $1`, actor.CredentialID); err != nil {
		return BrowserLogoutContext{}, err
	}
	revision, err := finalizeBrowserSessionMutation(ctx, tx, actor, actor.CredentialID, "user.browser_session.logout", "session", true)
	if err != nil {
		return BrowserLogoutContext{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BrowserLogoutContext{}, fmt.Errorf("commit browser session logout: %w", err)
	}
	logout.AuthorizationRevision = revision
	return logout, nil
}

func lockBrowserLogoutContext(ctx context.Context, tx pgx.Tx, actor accesscontrol.Actor, at time.Time) (BrowserLogoutContext, error) {
	var source string
	var result BrowserLogoutContext
	err := tx.QueryRow(ctx, `
		SELECT credential.source, COALESCE(logout.encrypted_dek, ''),
			COALESCE(logout.encrypted_logout_token, ''),
			COALESCE(logout.expires_at, 'epoch'::timestamptz)
		FROM fused_control_credentials credential
		LEFT JOIN fused_browser_logout_contexts logout
			ON logout.credential_id = credential.id AND logout.expires_at > $3
		WHERE credential.id = $1 AND credential.subject_id = $2
			AND credential.revoked_at IS NULL AND credential.expires_at > $3
			AND credential.source IN ('managed_login', 'license_exchange', 'api_key_exchange')
		FOR UPDATE OF credential
	`, actor.CredentialID, actor.SubjectID, at).Scan(
		&source, &result.EncryptedDEK, &result.EncryptedLogoutToken, &result.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return BrowserLogoutContext{}, accesscontrol.ErrAuthenticationRequired
	}
	if err != nil {
		return BrowserLogoutContext{}, err
	}
	// Only managed-login credentials may activate the Registry capability, even
	// if corrupt data were ever attached to another browser credential source.
	if source != "managed_login" {
		return BrowserLogoutContext{}, nil
	}
	return result, nil
}

func validBrowserSessionActor(actor accesscontrol.Actor) bool {
	return actor.SubjectID != uuid.Nil && actor.CredentialID != uuid.Nil
}

func validBrowserAuthMethod(method string) bool {
	return method == "license_key" || method == "api_key"
}

func browserSessionSource(authMethod string) string {
	if authMethod == "api_key" {
		return "api_key_exchange"
	}
	return "license_exchange"
}

func lockBrowserSessionSource(ctx context.Context, tx pgx.Tx, actor accesscontrol.Actor) error {
	var credentialID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT credential.id
		FROM fused_control_credentials credential
		JOIN fused_subjects subject ON subject.id = credential.subject_id
		WHERE credential.id = $1 AND credential.subject_id = $2
			AND credential.revoked_at IS NULL
			AND (credential.expires_at IS NULL OR credential.expires_at > NOW())
			AND subject.status = 'active'
		FOR UPDATE OF credential, subject
	`, actor.CredentialID, actor.SubjectID).Scan(&credentialID)
	if errors.Is(err, pgx.ErrNoRows) {
		return accesscontrol.ErrAuthenticationRequired
	}
	if err != nil {
		return fmt.Errorf("lock browser session source: %w", err)
	}
	return nil
}

func finalizeBrowserSessionMutation(
	ctx context.Context,
	tx pgx.Tx,
	actor accesscontrol.Actor,
	credentialID uuid.UUID,
	action, authMethod string,
	changed bool,
) (int64, error) {
	revision, err := bumpAuthorizationRevision(ctx, tx, changed)
	if err != nil {
		return 0, err
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, resource_type, resource_id,
			trace_id, outcome, metadata
		)
		SELECT $1, $2, $3, 'workspace', workspace.id, $4, 'succeeded',
			jsonb_build_object('session_credential_id', $5::text,
				'authorization_revision', $6::bigint, 'auth_method', $7::text)
		FROM fused_workspaces workspace WHERE workspace.singleton_key = 1
	`, actor.SubjectID, actor.CredentialID, action, accesscontrolTraceID(ctx), credentialID, revision, authMethod)
	if err != nil {
		return 0, fmt.Errorf("audit browser session: %w", err)
	}
	if command.RowsAffected() != 1 {
		return 0, ErrManagedLoginUnavailable
	}
	return revision, nil
}

var _ BrowserSessionStore = (*postgresStore)(nil)
