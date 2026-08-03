package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

func (s *postgresStore) CreateUser(ctx context.Context, input CreateUserInput) (UserMutationResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.user.create")
	defer span.End()
	normalized, display, err := validateCreateUserInput(input)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	tx, err := s.beginAccessMutation(ctx, input.Actor)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	user, err := insertInvitedUser(ctx, tx, normalized, display, input.DisplayName)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	user, err = loadHydratedUser(ctx, tx, user.ID)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, true)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := auditUserMutation(ctx, tx, input.Actor, "user.create", user.ID, revision, true); err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, fmt.Errorf("commit user create: %w", err))
	}
	span.SetAttributes(attribute.Bool("engine.access.changed", true), attribute.Int64("engine.authorization.revision", revision))
	return UserMutationResult{User: user, AuthorizationRevision: revision, Changed: true}, nil
}

func validateCreateUserInput(input CreateUserInput) (string, string, error) {
	normalized, display, err := normalizeUserEmail(input.Email)
	if err != nil {
		return "", "", err
	}
	if err := validateUserDisplayName(input.DisplayName); err != nil {
		return "", "", err
	}
	if err := validateMutationActor(input.Actor); err != nil {
		return "", "", err
	}
	return normalized, display, nil
}

func insertInvitedUser(ctx context.Context, tx pgx.Tx, normalized, email, displayName string) (User, error) {
	var user User
	err := tx.QueryRow(ctx, `
		WITH subject AS (
			INSERT INTO fused_subjects (kind, display_name, status)
			VALUES ('user', $3, 'invited') RETURNING id, display_name, status, created_at, updated_at
		), inserted AS (
			INSERT INTO fused_users (subject_id, email_normalized, email_display)
			SELECT id, $1, $2 FROM subject RETURNING subject_id, email_display, created_at, updated_at
		)
		SELECT inserted.subject_id, inserted.email_display, subject.display_name, subject.status,
			inserted.created_at, inserted.updated_at
		FROM inserted JOIN subject ON subject.id = inserted.subject_id
	`, normalized, email, displayName).Scan(&user.ID, &user.Email, &user.DisplayName, &user.Status, &user.CreatedAt, &user.UpdatedAt)
	if isUserEmailViolation(err) {
		return User{}, ErrUserEmailConflict
	}
	if err != nil {
		return User{}, fmt.Errorf("insert invited user: %w", err)
	}
	user.Memberships, user.Credentials = []TeamMembership{}, []ControlCredential{}
	return user, nil
}

func (s *postgresStore) UpdateUser(ctx context.Context, userID uuid.UUID, patch UserPatch) (UserMutationResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.user.update")
	defer span.End()
	normalized, email, err := validateUserPatch(userID, patch)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	tx, err := s.beginAccessMutation(ctx, patch.Actor)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	user, err := loadOwnerAuthorizedUser(ctx, tx, userID, patch.Actor.SubjectID)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	changed, err := applyAndPersistUserPatch(ctx, tx, &user, patch, normalized, email)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	user, err = loadHydratedUser(ctx, tx, user.ID)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, false)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := auditUserMutation(ctx, tx, patch.Actor, "user.update", user.ID, revision, changed); err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, fmt.Errorf("commit user update: %w", err))
	}
	return UserMutationResult{User: user, AuthorizationRevision: revision, Changed: changed}, nil
}

func applyAndPersistUserPatch(ctx context.Context, tx pgx.Tx, user *User, patch UserPatch, normalized, email *string) (bool, error) {
	if user.Status == UserStatusArchived {
		return false, ErrUserArchived
	}
	changed := applyUserPatch(user, patch, email)
	if !changed {
		return false, nil
	}
	return true, updateUserRow(ctx, tx, user, normalized)
}

func validateUserPatch(userID uuid.UUID, patch UserPatch) (*string, *string, error) {
	if userID == uuid.Nil || (patch.Email == nil && patch.DisplayName == nil) {
		return nil, nil, ErrInvalidUser
	}
	if err := validateMutationActor(patch.Actor); err != nil {
		return nil, nil, err
	}
	var normalized, email *string
	if patch.Email != nil {
		normalizedValue, displayValue, err := normalizeUserEmail(*patch.Email)
		if err != nil {
			return nil, nil, err
		}
		normalized, email = &normalizedValue, &displayValue
	}
	if patch.DisplayName != nil {
		if err := validateUserDisplayName(*patch.DisplayName); err != nil {
			return nil, nil, err
		}
	}
	return normalized, email, nil
}

func applyUserPatch(user *User, patch UserPatch, email *string) bool {
	changed := false
	if email != nil && user.Email != *email {
		user.Email, changed = *email, true
	}
	if patch.DisplayName != nil && user.DisplayName != *patch.DisplayName {
		user.DisplayName, changed = *patch.DisplayName, true
	}
	return changed
}

func updateUserRow(ctx context.Context, tx pgx.Tx, user *User, normalized *string) error {
	err := tx.QueryRow(ctx, `
		WITH updated_subject AS (
			UPDATE fused_subjects SET display_name = $2, updated_at = NOW() WHERE id = $1 RETURNING updated_at
		), updated_user AS (
			UPDATE fused_users SET email_normalized = COALESCE($3, email_normalized), email_display = $4, updated_at = NOW()
			WHERE subject_id = $1 RETURNING updated_at
		)
		SELECT updated_user.updated_at FROM updated_user CROSS JOIN updated_subject
	`, user.ID, user.DisplayName, normalized, user.Email).Scan(&user.UpdatedAt)
	if isUserEmailViolation(err) {
		return ErrUserEmailConflict
	}
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	return nil
}

func (s *postgresStore) SuspendUser(ctx context.Context, userID uuid.UUID, actor MutationActor) (UserMutationResult, error) {
	return s.setUserStatus(ctx, userID, UserStatusSuspended, actor)
}

func (s *postgresStore) ReactivateUser(ctx context.Context, userID uuid.UUID, actor MutationActor) (UserMutationResult, error) {
	return s.setUserStatus(ctx, userID, UserStatusActive, actor)
}

func (s *postgresStore) setUserStatus(ctx context.Context, userID uuid.UUID, target UserStatus, actor MutationActor) (UserMutationResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.user.status")
	defer span.End()
	if userID == uuid.Nil || (target != UserStatusSuspended && target != UserStatusActive) {
		return UserMutationResult{}, ErrInvalidUser
	}
	tx, err := s.beginAccessMutation(ctx, actor)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := mutateUserStatusTx(ctx, tx, userID, target, actor)
	if err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return UserMutationResult{}, recordTeamSpanError(span, fmt.Errorf("commit user status: %w", err))
	}
	return result, nil
}

func mutateUserStatusTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, target UserStatus, actor MutationActor) (UserMutationResult, error) {
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return UserMutationResult{}, err
	}
	user, err := loadOwnerAuthorizedUser(ctx, tx, userID, actor.SubjectID)
	if err != nil {
		return UserMutationResult{}, err
	}
	changed, err := changeUserStatus(ctx, tx, &user, target)
	if err != nil {
		return UserMutationResult{}, err
	}
	user, err = loadHydratedUser(ctx, tx, user.ID)
	if err != nil {
		return UserMutationResult{}, err
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, changed)
	if err != nil {
		return UserMutationResult{}, err
	}
	action := userStatusAction(target)
	if err := auditUserMutation(ctx, tx, actor, action, user.ID, revision, changed); err != nil {
		return UserMutationResult{}, err
	}
	return UserMutationResult{User: user, AuthorizationRevision: revision, Changed: changed}, nil
}

func userStatusAction(target UserStatus) string {
	if target == UserStatusSuspended {
		return "user.suspend"
	}
	return "user.reactivate"
}

func changeUserStatus(ctx context.Context, tx pgx.Tx, user *User, target UserStatus) (bool, error) {
	if user.Status == UserStatusArchived {
		return false, ErrUserArchived
	}
	// Invitations become active only when their first credential is issued;
	// reactivate is reserved for previously suspended principals.
	if user.Status == UserStatusInvited && target == UserStatusActive {
		return false, ErrInvalidUser
	}
	if user.Status == target {
		return false, nil
	}
	if err := updateUserStatus(ctx, tx, user, target); err != nil {
		return false, err
	}
	if err := ensureEffectiveOwnerRemains(ctx, tx); err != nil {
		return false, err
	}
	return true, nil
}

func loadUserForMutation(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (User, error) {
	var user User
	err := tx.QueryRow(ctx, `
		SELECT user_row.subject_id, user_row.email_display, subject.display_name, subject.status,
			user_row.created_at, user_row.updated_at
		FROM fused_users user_row JOIN fused_subjects subject ON subject.id = user_row.subject_id
		WHERE user_row.subject_id = $1 FOR UPDATE OF subject, user_row
	`, userID).Scan(&user.ID, &user.Email, &user.DisplayName, &user.Status, &user.CreatedAt, &user.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("load user for mutation: %w", err)
	}
	user.Memberships, user.Credentials = []TeamMembership{}, []ControlCredential{}
	return user, nil
}

func updateUserStatus(ctx context.Context, tx pgx.Tx, user *User, status UserStatus) error {
	if _, err := tx.Exec(ctx, `UPDATE fused_subjects SET status = $2, updated_at = NOW() WHERE id = $1`, user.ID, status); err != nil {
		return fmt.Errorf("update user status: %w", err)
	}
	user.Status, user.UpdatedAt = status, time.Now().UTC()
	return nil
}

func lockAuthorizationState(ctx context.Context, tx pgx.Tx) (int64, error) {
	var revision int64
	// Every grant-affecting mutation takes this singleton row lock. Owner safety,
	// target protection, the mutation, revision bump, and audit therefore observe
	// one serial authorization order across Engine processes.
	if err := tx.QueryRow(ctx, `SELECT revision FROM fused_authorization_state WHERE singleton_key = 1 FOR UPDATE`).Scan(&revision); err != nil {
		return 0, fmt.Errorf("lock authorization state: %w", err)
	}
	return revision, nil
}

func ensureEffectiveOwnerRemains(ctx context.Context, tx pgx.Tx) error {
	var count int
	if err := tx.QueryRow(ctx, effectiveOwnerCountSQL).Scan(&count); err != nil {
		return fmt.Errorf("count effective Owners: %w", err)
	}
	if count == 0 {
		return ErrLastEffectiveOwner
	}
	return nil
}

const effectiveOwnerCountSQL = `
	WITH owner_subjects AS (
		SELECT subject.id
		FROM fused_subjects subject
		JOIN fused_role_bindings binding ON binding.subject_type = 'subject' AND binding.subject_id = subject.id
		JOIN fused_roles role ON role.id = binding.role_id AND role.slug = 'owner' AND role.scope_type = 'workspace'
		JOIN fused_workspaces workspace ON workspace.id = binding.resource_id AND workspace.singleton_key = 1
		WHERE subject.status = 'active'
		UNION
		SELECT subject.id
		FROM fused_subjects subject
		JOIN fused_team_memberships membership ON membership.member_subject_id = subject.id
		JOIN fused_teams team ON team.id = membership.team_id AND team.status = 'active'
		JOIN fused_role_bindings binding ON binding.subject_type = 'team' AND binding.subject_id = team.id
		JOIN fused_roles role ON role.id = binding.role_id AND role.slug = 'owner' AND role.scope_type = 'workspace'
		JOIN fused_workspaces workspace ON workspace.id = binding.resource_id AND workspace.singleton_key = 1
		WHERE subject.status = 'active'
	)
	SELECT COUNT(DISTINCT id) FROM owner_subjects
`

func auditUserMutation(ctx context.Context, tx pgx.Tx, actor MutationActor, action string, userID uuid.UUID, revision int64, changed bool) error {
	workspaceID, err := workspaceIDForAudit(ctx, tx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, permission, resource_type, resource_id,
			request_id, trace_id, outcome, metadata
		) VALUES ($1, $2, $3, 'access.manage', 'workspace', $4, $5, $6, 'succeeded',
			jsonb_build_object('user_id', $7::text, 'authorization_revision', $8::bigint, 'changed', $9::boolean))
	`, actor.SubjectID, actor.CredentialID, action, workspaceID, actor.RequestID, actor.TraceID, userID.String(), revision, changed)
	if err != nil {
		return fmt.Errorf("audit user mutation: %w", err)
	}
	return nil
}

func workspaceIDForAudit(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
	var workspaceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM fused_workspaces WHERE singleton_key = 1`).Scan(&workspaceID); err != nil {
		return uuid.Nil, fmt.Errorf("load workspace for audit: %w", err)
	}
	return workspaceID, nil
}

func isUserEmailViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505" && postgresError.ConstraintName == "fused_users_email_normalized_key"
}

func generatedUserDisplayName(email string) string {
	local, _, _ := strings.Cut(email, "@")
	local = strings.TrimSpace(local)
	if local == "" {
		return "Invited user"
	}
	if len(local) > 100 {
		return local[:100]
	}
	return local
}

func generateControlCredential() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate control credential: %w", err)
	}
	return "fsk_" + base64.RawURLEncoding.EncodeToString(random), nil
}
