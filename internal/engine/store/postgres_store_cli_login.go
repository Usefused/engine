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

type lockedCLILogin struct {
	state               string
	credentialHash      string
	credentialPrefix    string
	authMethod          string
	subjectID           uuid.UUID
	actorCredentialID   uuid.UUID
	credentialID        uuid.UUID
	expiresAt           time.Time
	credentialExpiresAt time.Time
}

func (s *postgresStore) CreateCLILoginTransaction(ctx context.Context, transaction CLILoginTransaction) error {
	command, err := s.db.Exec(ctx, `
		INSERT INTO fused_cli_login_transactions (
			id, poll_secret_hash, browser_secret_hash, credential_hash,
			credential_prefix, expires_at, credential_expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, transaction.ID, transaction.PollSecretHash, transaction.BrowserSecretHash,
		transaction.CredentialHash, transaction.CredentialPrefix,
		transaction.ExpiresAt, transaction.CredentialExpiresAt)
	if err != nil {
		return fmt.Errorf("create CLI login transaction: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrCLILoginUnavailable
	}
	return nil
}

func (s *postgresStore) ApproveCLILoginTransaction(
	ctx context.Context,
	id uuid.UUID,
	browserHash string,
	actor accesscontrol.Actor,
	at time.Time,
) error {
	command, err := s.db.Exec(ctx, `
		UPDATE fused_cli_login_transactions transaction
		SET state = 'approved', approved_subject_id = credential.subject_id,
			approved_by_credential_id = credential.id,
			auth_method = credential.auth_method, approved_at = $5
		FROM fused_control_credentials credential
		JOIN fused_subjects subject ON subject.id = credential.subject_id
		WHERE transaction.id = $1 AND transaction.browser_secret_hash = $2
			AND transaction.state = 'pending' AND transaction.expires_at > $5
			AND credential.id = $3 AND credential.subject_id = $4
			AND credential.revoked_at IS NULL
			AND (credential.expires_at IS NULL OR credential.expires_at > $5)
			AND credential.source IN ('managed_login', 'license_exchange', 'api_key_exchange')
			AND subject.status = 'active'
	`, id, browserHash, actor.CredentialID, actor.SubjectID, at)
	if err != nil {
		return fmt.Errorf("approve CLI login transaction: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrCLILoginDenied
	}
	return nil
}

func (s *postgresStore) ConsumeCLILoginTransaction(ctx context.Context, id uuid.UUID, pollHash string, at time.Time) (CLILoginCredential, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CLILoginCredential{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := consumeCLILoginInTransaction(ctx, tx, id, pollHash, at)
	if err != nil {
		return CLILoginCredential{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CLILoginCredential{}, fmt.Errorf("commit CLI login: %w", err)
	}
	return result, nil
}

func (s *postgresStore) RevokeCurrentCLICredential(ctx context.Context, actor MutationActor) (CLILogoutResult, error) {
	tx, err := s.beginAccessMutation(ctx, actor)
	if err != nil {
		return CLILogoutResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Logout is deliberately self-scoped: browser and bootstrap credentials use
	// separate lifecycles and must never be revocable through the CLI endpoint.
	command, err := tx.Exec(ctx, `
		UPDATE fused_control_credentials
		SET revoked_at = NOW()
		WHERE id = $1 AND subject_id = $2 AND source = 'managed_cli_login'
			AND revoked_at IS NULL
	`, actor.CredentialID, actor.SubjectID)
	if err != nil {
		return CLILogoutResult{}, fmt.Errorf("revoke current CLI credential: %w", err)
	}
	if command.RowsAffected() != 1 {
		return CLILogoutResult{}, ErrCLILogoutDenied
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, true)
	if err != nil {
		return CLILogoutResult{}, err
	}
	if err := auditCLILogout(ctx, tx, actor, revision); err != nil {
		return CLILogoutResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CLILogoutResult{}, fmt.Errorf("commit CLI logout: %w", err)
	}
	return CLILogoutResult{AuthorizationRevision: revision}, nil
}

func auditCLILogout(ctx context.Context, tx pgx.Tx, actor MutationActor, revision int64) error {
	command, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, resource_type,
			resource_id, request_id, trace_id, outcome, metadata
		)
		SELECT $1::uuid, $2::uuid, 'user.cli_credential.revoke', 'workspace', workspace.id,
			$3, $4, 'succeeded', jsonb_build_object(
				'credential_id', ($2::uuid)::text,
				'credential_source', 'managed_cli_login',
				'authorization_revision', $5::bigint,
				'client_type', 'cli'
			)
		FROM fused_workspaces workspace
		WHERE workspace.singleton_key = 1
	`, actor.SubjectID, actor.CredentialID, actor.RequestID, actor.TraceID, revision)
	if err != nil {
		return fmt.Errorf("audit CLI logout: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrCLILoginUnavailable
	}
	return nil
}

func consumeCLILoginInTransaction(ctx context.Context, tx pgx.Tx, id uuid.UUID, pollHash string, at time.Time) (CLILoginCredential, error) {
	transaction, err := lockCLILogin(ctx, tx, id, pollHash, at)
	if err != nil {
		return CLILoginCredential{}, err
	}
	if transaction.state == CLILoginStateConsumed {
		return loadConsumedCLICredential(ctx, tx, transaction)
	}
	if transaction.state != CLILoginStateApproved {
		return CLILoginCredential{}, ErrCLILoginPending
	}
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return CLILoginCredential{}, err
	}
	credentialID, err := insertCLICredential(ctx, tx, id, transaction)
	if err != nil {
		return CLILoginCredential{}, err
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, true)
	if err != nil {
		return CLILoginCredential{}, err
	}
	if err := auditCLICredential(ctx, tx, transaction, credentialID, revision); err != nil {
		return CLILoginCredential{}, err
	}
	if err := markCLILoginConsumed(ctx, tx, id, credentialID, at); err != nil {
		return CLILoginCredential{}, err
	}
	return CLILoginCredential{
		SubjectID: transaction.subjectID, CredentialID: credentialID,
		ExpiresAt: transaction.credentialExpiresAt, AuthorizationRevision: revision,
	}, nil
}

func lockCLILogin(ctx context.Context, tx pgx.Tx, id uuid.UUID, pollHash string, at time.Time) (lockedCLILogin, error) {
	var result lockedCLILogin
	err := tx.QueryRow(ctx, `
		SELECT state, credential_hash, credential_prefix, COALESCE(auth_method, ''),
			COALESCE(approved_subject_id, '00000000-0000-0000-0000-000000000000'::uuid),
			COALESCE(approved_by_credential_id, '00000000-0000-0000-0000-000000000000'::uuid),
			COALESCE(credential_id, '00000000-0000-0000-0000-000000000000'::uuid),
			expires_at, credential_expires_at
		FROM fused_cli_login_transactions
		WHERE id = $1 AND poll_secret_hash = $2
		FOR UPDATE
	`, id, pollHash).Scan(
		&result.state, &result.credentialHash, &result.credentialPrefix,
		&result.authMethod, &result.subjectID, &result.actorCredentialID,
		&result.credentialID, &result.expiresAt, &result.credentialExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedCLILogin{}, ErrCLILoginUnavailable
	}
	if err != nil {
		return lockedCLILogin{}, fmt.Errorf("lock CLI login transaction: %w", err)
	}
	if !result.expiresAt.After(at) && result.state != CLILoginStateConsumed {
		return lockedCLILogin{}, ErrCLILoginUnavailable
	}
	return result, nil
}

func insertCLICredential(ctx context.Context, tx pgx.Tx, transactionID uuid.UUID, login lockedCLILogin) (uuid.UUID, error) {
	var credentialID uuid.UUID
	name := fmt.Sprintf("CLI %s %s", login.credentialPrefix, transactionID.String()[:8])
	err := tx.QueryRow(ctx, `
		INSERT INTO fused_control_credentials (
			subject_id, key_hash, key_prefix, name, expires_at, source, auth_method
		)
		VALUES ($1, $2, $3, $4, $5, 'managed_cli_login', $6)
		RETURNING id
	`, login.subjectID, login.credentialHash, login.credentialPrefix, name,
		login.credentialExpiresAt, login.authMethod).Scan(&credentialID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert CLI credential: %w", err)
	}
	return credentialID, nil
}

func auditCLICredential(ctx context.Context, tx pgx.Tx, login lockedCLILogin, credentialID uuid.UUID, revision int64) error {
	command, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, resource_type,
			resource_id, trace_id, outcome, metadata
		)
		SELECT $1, $2, 'user.cli_credential.create', 'workspace', workspace.id,
			$3, 'succeeded', jsonb_build_object(
				'credential_id', $4::text,
				'authorization_revision', $5::bigint,
				'auth_method', $6::text,
				'client_type', 'cli'
			)
		FROM fused_workspaces workspace WHERE workspace.singleton_key = 1
	`, login.subjectID, login.actorCredentialID, accesscontrolTraceID(ctx),
		credentialID, revision, login.authMethod)
	if err != nil {
		return fmt.Errorf("audit CLI credential: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrCLILoginUnavailable
	}
	return nil
}

func markCLILoginConsumed(ctx context.Context, tx pgx.Tx, id, credentialID uuid.UUID, at time.Time) error {
	command, err := tx.Exec(ctx, `
		UPDATE fused_cli_login_transactions
		SET state = 'consumed', credential_id = $2, consumed_at = $3
		WHERE id = $1 AND state = 'approved'
	`, id, credentialID, at)
	if err != nil {
		return fmt.Errorf("consume CLI login transaction: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrCLILoginUnavailable
	}
	return nil
}

func loadConsumedCLICredential(ctx context.Context, tx pgx.Tx, login lockedCLILogin) (CLILoginCredential, error) {
	if login.credentialID == uuid.Nil {
		return CLILoginCredential{}, ErrCLILoginUnavailable
	}
	var result CLILoginCredential
	err := tx.QueryRow(ctx, `
		SELECT credential.subject_id, credential.id, credential.expires_at, state.revision
		FROM fused_control_credentials credential
		CROSS JOIN fused_authorization_state state
		WHERE credential.id = $1 AND credential.subject_id = $2
			AND credential.key_hash = $3 AND state.singleton_key = 1
	`, login.credentialID, login.subjectID, login.credentialHash).Scan(
		&result.SubjectID, &result.CredentialID, &result.ExpiresAt,
		&result.AuthorizationRevision,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CLILoginCredential{}, ErrCLILoginUnavailable
	}
	return result, err
}

var _ CLILoginStore = (*postgresStore)(nil)
