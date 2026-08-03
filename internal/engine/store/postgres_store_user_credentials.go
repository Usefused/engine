package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func (s *postgresStore) IssueUserControlCredential(ctx context.Context, input IssueCredentialInput) (IssuedControlCredential, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.user.credential.issue")
	defer span.End()
	if err := validateIssueCredentialInput(input); err != nil {
		return IssuedControlCredential{}, recordTeamSpanError(span, err)
	}
	rawKey, err := generateControlCredential()
	if err != nil {
		return IssuedControlCredential{}, recordTeamSpanError(span, err)
	}
	tx, err := s.beginAccessMutation(ctx, input.Actor)
	if err != nil {
		return IssuedControlCredential{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return IssuedControlCredential{}, recordTeamSpanError(span, err)
	}
	user, err := loadOwnerAuthorizedUser(ctx, tx, input.UserID, input.Actor.SubjectID)
	if err != nil {
		return IssuedControlCredential{}, recordTeamSpanError(span, err)
	}
	if err := activateUserForCredential(ctx, tx, &user); err != nil {
		return IssuedControlCredential{}, recordTeamSpanError(span, err)
	}
	credential, err := insertUserControlCredential(ctx, tx, input, rawKey)
	if err != nil {
		return IssuedControlCredential{}, recordTeamSpanError(span, err)
	}
	revision, err := finalizeCredentialMutation(ctx, tx, input.Actor, "user.credential.issue", credential, true)
	if err != nil {
		return IssuedControlCredential{}, recordTeamSpanError(span, err)
	}
	return IssuedControlCredential{Credential: credential, RawKey: rawKey, AuthorizationRevision: revision, Changed: true}, nil
}

func activateUserForCredential(ctx context.Context, tx pgx.Tx, user *User) error {
	if user.Status != UserStatusInvited && user.Status != UserStatusActive {
		return ErrInvalidControlCredential
	}
	if user.Status == UserStatusActive {
		return nil
	}
	return updateUserStatus(ctx, tx, user, UserStatusActive)
}

func validateIssueCredentialInput(input IssueCredentialInput) error {
	if input.UserID == uuid.Nil || validateUserDisplayName(input.Name) != nil {
		return ErrInvalidControlCredential
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now()) {
		return ErrInvalidControlCredential
	}
	return validateMutationActor(input.Actor)
}

func insertUserControlCredential(ctx context.Context, tx pgx.Tx, input IssueCredentialInput, rawKey string) (ControlCredential, error) {
	var credential ControlCredential
	err := tx.QueryRow(ctx, `
		INSERT INTO fused_control_credentials (subject_id, key_hash, key_prefix, name, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, subject_id, key_prefix, name, expires_at, last_used_at, revoked_at, created_at
	`, input.UserID, accesscontrol.HashControlCredential(rawKey), accesscontrol.CredentialPrefix(rawKey), input.Name, input.ExpiresAt).Scan(
		&credential.ID, &credential.UserID, &credential.KeyPrefix, &credential.Name,
		&credential.ExpiresAt, &credential.LastUsedAt, &credential.RevokedAt, &credential.CreatedAt,
	)
	if err != nil {
		return ControlCredential{}, fmt.Errorf("insert user control credential: %w", err)
	}
	return credential, nil
}

func (s *postgresStore) RevokeUserControlCredential(ctx context.Context, userID, credentialID uuid.UUID, actor MutationActor) (CredentialMutationResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.user.credential.revoke")
	defer span.End()
	if userID == uuid.Nil || credentialID == uuid.Nil || validateMutationActor(actor) != nil {
		return CredentialMutationResult{}, ErrInvalidControlCredential
	}
	tx, err := s.beginAccessMutation(ctx, actor)
	if err != nil {
		return CredentialMutationResult{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return CredentialMutationResult{}, recordTeamSpanError(span, err)
	}
	_, err = loadOwnerAuthorizedUser(ctx, tx, userID, actor.SubjectID)
	if err != nil {
		return CredentialMutationResult{}, recordTeamSpanError(span, err)
	}
	credential, changed, err := revokeUserControlCredential(ctx, tx, userID, credentialID)
	if err != nil {
		return CredentialMutationResult{}, recordTeamSpanError(span, err)
	}
	revision, err := finalizeCredentialMutation(ctx, tx, actor, "user.credential.revoke", credential, changed)
	if err != nil {
		return CredentialMutationResult{}, recordTeamSpanError(span, err)
	}
	return CredentialMutationResult{Credential: credential, AuthorizationRevision: revision, Changed: changed}, nil
}

func finalizeCredentialMutation(ctx context.Context, tx pgx.Tx, actor MutationActor, action string, credential ControlCredential, changed bool) (int64, error) {
	revision, err := bumpAuthorizationRevision(ctx, tx, changed)
	if err != nil {
		return 0, err
	}
	if err := auditCredentialMutation(ctx, tx, actor, action, credential, revision, changed); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit credential mutation: %w", err)
	}
	return revision, nil
}

func revokeUserControlCredential(ctx context.Context, tx pgx.Tx, userID, credentialID uuid.UUID) (ControlCredential, bool, error) {
	credential, err := loadUserControlCredentialForMutation(ctx, tx, userID, credentialID)
	if err != nil {
		return ControlCredential{}, false, err
	}
	if credential.RevokedAt != nil {
		return credential, false, nil
	}
	var revokedAt time.Time
	if err := tx.QueryRow(ctx, `UPDATE fused_control_credentials SET revoked_at = NOW() WHERE id = $1 RETURNING revoked_at`, credential.ID).Scan(&revokedAt); err != nil {
		return ControlCredential{}, false, fmt.Errorf("revoke control credential: %w", err)
	}
	credential.RevokedAt = &revokedAt
	return credential, true, nil
}

func loadUserControlCredentialForMutation(ctx context.Context, tx pgx.Tx, userID, credentialID uuid.UUID) (ControlCredential, error) {
	credential, err := scanControlCredential(tx.QueryRow(ctx, `
		SELECT id, subject_id, key_prefix, name, expires_at, last_used_at, revoked_at, created_at
		FROM fused_control_credentials WHERE id = $1 AND subject_id = $2 FOR UPDATE
	`, credentialID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ControlCredential{}, ErrControlCredentialNotFound
	}
	return credential, err
}

func auditCredentialMutation(ctx context.Context, tx pgx.Tx, actor MutationActor, action string, credential ControlCredential, revision int64, changed bool) error {
	workspaceID, err := workspaceIDForAudit(ctx, tx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, permission, resource_type, resource_id,
			request_id, trace_id, outcome, metadata
		) VALUES ($1, $2, $3, 'access.manage', 'workspace', $4, $5, $6, 'succeeded',
			jsonb_build_object('user_id', $7::text, 'credential_id', $8::text,
				'authorization_revision', $9::bigint, 'changed', $10::boolean))
	`, actor.SubjectID, actor.CredentialID, action, workspaceID, actor.RequestID, actor.TraceID,
		credential.UserID.String(), credential.ID.String(), revision, changed)
	if err != nil {
		return fmt.Errorf("audit credential mutation: %w", err)
	}
	return nil
}
