package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type appTokenBindingPayload struct {
	ServiceSlug string     `json:"service_slug"`
	AuthName    string     `json:"auth_name"`
	EndUserRef  string     `json:"end_user_ref"`
	ResourceID  *uuid.UUID `json:"resource_id"`
}

// CreateAppToken writes the revocable credential and retained lifecycle row in
// one transaction. A failed binding resolution therefore cannot leave either
// a usable token or misleading audit history behind.
func (s *postgresStore) CreateAppToken(ctx context.Context, issue AppTokenIssue) (*AppTokenMetadata, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.store.app_token.create")
	defer span.End()
	span.SetAttributes(
		attribute.String("app.family_id", issue.AppFamilyID.String()),
		attribute.String("app.token.binding_mode", string(issue.BindingMode)),
		attribute.Int("app.token.binding_count", len(issue.Bindings)),
	)
	if err := validateAppTokenIssue(issue); err != nil {
		span.SetAttributes(attribute.String("outcome", "invalid"))
		return nil, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create app token transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	metadata, err := createAppTokenTx(ctx, tx, issue)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return nil, fmt.Errorf("commit app token: %w", err)
	}
	span.SetAttributes(attribute.String("outcome", "created"))
	return metadata, nil
}

func validateAppTokenIssue(issue AppTokenIssue) error {
	if issue.ID == uuid.Nil || issue.AppFamilyID == uuid.Nil {
		return errors.New("app token identity is required")
	}
	if strings.TrimSpace(issue.TokenHash) == "" || strings.TrimSpace(issue.Name) == "" {
		return errors.New("app token credential and name are required")
	}
	if !issue.BindingMode.Valid() {
		return errors.New("app token binding mode is invalid")
	}
	return validateAppTokenBindingRequests(issue.BindingMode, issue.Bindings)
}

func validateAppTokenBindingRequests(mode AppTokenBindingMode, bindings []AppTokenBindingRequest) error {
	if mode == AppTokenBindingDynamic && len(bindings) > 0 {
		return ErrAppTokenBindingInvalid
	}
	if mode == AppTokenBindingFixed && len(bindings) == 0 {
		return ErrAppTokenBindingInvalid
	}
	seen := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if err := validateAppTokenBindingRequest(binding, seen); err != nil {
			return err
		}
	}
	return nil
}

func validateAppTokenBindingRequest(binding AppTokenBindingRequest, seen map[string]struct{}) error {
	serviceSlug := strings.TrimSpace(binding.ServiceSlug)
	if !validServiceSlugReference(serviceSlug) || strings.TrimSpace(binding.AuthName) == "" || strings.TrimSpace(binding.EndUserRef) == "" {
		return ErrAppTokenBindingInvalid
	}
	if binding.ResourceID != nil && *binding.ResourceID == uuid.Nil {
		return ErrAppTokenBindingInvalid
	}
	// Exact repeats can fail before opening a transaction. Qualified aliases are
	// deliberately resolved by the shared database resolver, which also detects
	// when two different spellings target the same service/auth pair.
	key := strings.ToLower(serviceSlug) + "\x00" + binding.AuthName
	if _, duplicate := seen[key]; duplicate {
		return ErrAppTokenBindingInvalid
	}
	seen[key] = struct{}{}
	return nil
}

// validServiceSlugReference rejects internal UUIDs and malformed provider
// qualifiers before a user-controlled value reaches workspace resolution.
func validServiceSlugReference(reference string) bool {
	if reference == "" || strings.ContainsAny(reference, " \t\r\n,") {
		return false
	}
	if _, err := uuid.Parse(reference); err == nil {
		return false
	}
	if !strings.HasPrefix(reference, "@") {
		return !strings.Contains(reference, "/")
	}
	remainder := reference[1:]
	slash := strings.IndexByte(remainder, '/')
	return slash > 0 && slash < len(remainder)-1 && !strings.Contains(remainder[slash+1:], "/")
}

func createAppTokenTx(ctx context.Context, tx pgx.Tx, issue AppTokenIssue) (*AppTokenMetadata, error) {
	metadata, err := insertAppTokenHistory(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	if err := insertActiveAppToken(ctx, tx, issue); err != nil {
		return nil, err
	}
	if err := insertAppTokenBindings(ctx, tx, issue); err != nil {
		return nil, err
	}
	if err := auditAppTokenMutation(ctx, tx, issue, "app.token.generate", len(issue.Bindings)); err != nil {
		return nil, err
	}
	return metadata, nil
}

func insertAppTokenHistory(ctx context.Context, tx pgx.Tx, issue AppTokenIssue) (*AppTokenMetadata, error) {
	var token AppTokenMetadata
	// Active history rows intentionally retain a NULL termination reason, while
	// the secret-free metadata DTO exposes absence as its empty string value.
	err := tx.QueryRow(ctx, `
		INSERT INTO fused_app_token_history
			(id, app_family_id, name, allow_all, allowed_operations, expires_at,
			 binding_mode, issued_by_subject_id, issued_by_credential_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, app_family_id, name, allow_all, allowed_operations, expires_at,
		          binding_mode, status, issued_by_subject_id, issued_by_credential_id,
		          terminated_at, COALESCE(termination_reason, ''), created_at
	`, issue.ID, issue.AppFamilyID, issue.Name, issue.Policy.AllowAll,
		nonNilStrings(issue.Policy.AllowedOperations), issue.Policy.ExpiresAt, issue.BindingMode,
		issue.IssuedBySubjectID, issue.IssuedByCredentialID).Scan(
		&token.ID, &token.AppFamilyID, &token.Name, &token.AllowAll,
		&token.AllowedOperations, &token.ExpiresAt, &token.BindingMode,
		&token.Status, &token.IssuedBySubjectID, &token.IssuedByCredentialID,
		&token.TerminatedAt, &token.TerminationReason, &token.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create app token history: %w", err)
	}
	return &token, nil
}

func insertActiveAppToken(ctx context.Context, tx pgx.Tx, issue AppTokenIssue) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_app_tokens
			(id, app_family_id, token_hash, name, allow_all, allowed_operations, expires_at, binding_mode)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, issue.ID, issue.AppFamilyID, issue.TokenHash, issue.Name, issue.Policy.AllowAll,
		nonNilStrings(issue.Policy.AllowedOperations), issue.Policy.ExpiresAt, issue.BindingMode)
	if err != nil {
		return fmt.Errorf("create active app token: %w", err)
	}
	return nil
}

var insertAppTokenBindingsSQL = `
	WITH requested AS (
		SELECT binding.service_slug AS key, binding.auth_name,
		       binding.end_user_ref, binding.resource_id
		FROM jsonb_to_recordset($2::jsonb) AS binding(
			service_slug text, auth_name text, end_user_ref text, resource_id uuid)
	), service_refs AS (` + workspaceServiceResolutionSQL(`requested input`, false) + `
	), resolved AS (
		SELECT $1::uuid AS token_id, service_ref.service_id, input.auth_name,
		       connection.id AS auth_connection_id, resource.id AS resource_id
		FROM requested input
		JOIN fused_app_tokens token ON token.id = $1
		JOIN fused_app_family_buckets family_bucket ON family_bucket.app_family_id = token.app_family_id
		JOIN service_refs service_ref ON service_ref.key = input.key
		JOIN fused_auth_connections connection
		  ON connection.bucket_id = family_bucket.bucket_id
		 AND connection.service_id = service_ref.service_id
		 AND connection.end_user_ref = input.end_user_ref
		 AND connection.auth_name = input.auth_name
		LEFT JOIN fused_connection_resources resource
		  ON resource.id = input.resource_id
		 AND resource.connection_id = connection.id
		 AND resource.is_active
		WHERE input.resource_id IS NULL OR resource.id IS NOT NULL
	), resolved_counts AS (
		SELECT resolved.*,
		       COUNT(*) OVER (PARTITION BY token_id, service_id, auth_name) AS pair_count
		FROM resolved
	), persisted AS (
		INSERT INTO fused_app_token_bindings
			(token_id, service_id, auth_name, auth_connection_id, resource_id)
		SELECT token_id, service_id, auth_name, auth_connection_id, resource_id
		FROM resolved_counts WHERE pair_count = 1
		RETURNING 1
	)
	SELECT (SELECT COUNT(*) FROM requested), (SELECT COUNT(*) FROM persisted)`

func insertAppTokenBindings(ctx context.Context, tx pgx.Tx, issue AppTokenIssue) error {
	if len(issue.Bindings) == 0 {
		return nil
	}
	payload, err := marshalAppTokenBindings(issue.Bindings)
	if err != nil {
		return err
	}
	var requested, inserted int
	err = tx.QueryRow(ctx, insertAppTokenBindingsSQL, issue.ID, payload).Scan(&requested, &inserted)
	if err != nil {
		return fmt.Errorf("create app token bindings: %w", err)
	}
	if requested != len(issue.Bindings) || inserted != requested {
		return ErrAppTokenBindingInvalid
	}
	return nil
}

func marshalAppTokenBindings(bindings []AppTokenBindingRequest) ([]byte, error) {
	payload := make([]appTokenBindingPayload, len(bindings))
	for index, binding := range bindings {
		payload[index] = appTokenBindingPayload(binding)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode app token bindings: %w", err)
	}
	return encoded, nil
}

func (s *postgresStore) ListAppTokens(ctx context.Context, appFamilyID uuid.UUID) ([]AppTokenMetadata, error) {
	// Active history has no termination reason by design; normalize that NULL
	// at the SQL projection boundary so every metadata scan uses one wire shape.
	rows, err := s.db.Query(ctx, `
		WITH family_tokens AS (
			SELECT id FROM fused_app_token_history WHERE app_family_id = $1
		), execution_usage AS (
			SELECT event.app_token_id AS token_id, COUNT(*) AS use_count,
			       MAX(event.started_at) AS last_used_at
			FROM fused_engine_execution_events event
			JOIN family_tokens token ON token.id = event.app_token_id
			GROUP BY event.app_token_id
		), session_usage AS (
			SELECT session.app_token_id AS token_id, COUNT(*) AS session_count,
			       MAX(session.last_activity_at) AS last_used_at
			FROM fused_mcp_sessions session
			JOIN family_tokens token ON token.id = session.app_token_id
			GROUP BY session.app_token_id
		)
		SELECT history.id, history.app_family_id, history.name, history.allow_all,
		       history.allowed_operations, history.expires_at, history.binding_mode,
		       CASE WHEN history.status = 'active' AND history.expires_at <= NOW()
		            THEN 'expired' ELSE history.status END,
		       history.issued_by_subject_id, history.issued_by_credential_id,
		       GREATEST(active.last_used_at, execution_usage.last_used_at, session_usage.last_used_at),
		       COALESCE(execution_usage.use_count, 0), COALESCE(session_usage.session_count, 0),
		       CASE WHEN history.status = 'active' AND history.expires_at <= NOW()
		            THEN history.expires_at ELSE history.terminated_at END,
		       CASE WHEN history.status = 'active' AND history.expires_at <= NOW()
		            THEN 'expired' ELSE COALESCE(history.termination_reason, '') END,
		       history.created_at
		FROM fused_app_token_history history
		LEFT JOIN fused_app_tokens active ON active.id = history.id
		LEFT JOIN execution_usage ON execution_usage.token_id = history.id
		LEFT JOIN session_usage ON session_usage.token_id = history.id
		WHERE history.app_family_id = $1
		ORDER BY history.created_at DESC
		LIMIT $2
	`, appFamilyID, appFamilyCollectionLimit)
	if err != nil {
		return nil, fmt.Errorf("list app tokens: %w", err)
	}
	defer rows.Close()
	return scanAppTokenMetadataRows(rows)
}

func scanAppTokenMetadataRows(rows pgx.Rows) ([]AppTokenMetadata, error) {
	tokens := make([]AppTokenMetadata, 0)
	for rows.Next() {
		var token AppTokenMetadata
		if err := rows.Scan(
			&token.ID, &token.AppFamilyID, &token.Name, &token.AllowAll,
			&token.AllowedOperations, &token.ExpiresAt, &token.BindingMode,
			&token.Status, &token.IssuedBySubjectID, &token.IssuedByCredentialID,
			&token.LastUsedAt, &token.ExecutionCount, &token.SessionCount,
			&token.TerminatedAt, &token.TerminationReason,
			&token.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan app token: %w", err)
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *postgresStore) RevokeAppToken(ctx context.Context, appFamilyID uuid.UUID, name string) (*AppTokenRevocation, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.store.app_token.revoke")
	defer span.End()
	span.SetAttributes(attribute.String("app.family_id", appFamilyID.String()))
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("revoke app token transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	revocation, err := revokeAppTokenTx(ctx, tx, appFamilyID, name)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		span.SetAttributes(attribute.String("outcome", "failed"))
		return nil, fmt.Errorf("commit app token revocation: %w", err)
	}
	span.SetAttributes(attribute.String("outcome", "revoked"))
	return revocation, nil
}

func revokeAppTokenTx(ctx context.Context, tx pgx.Tx, appFamilyID uuid.UUID, name string) (*AppTokenRevocation, error) {
	actor, _ := accesscontrol.ActorFromContext(ctx)
	var revocation AppTokenRevocation
	err := tx.QueryRow(ctx, `
		WITH target AS (
			SELECT id, app_family_id FROM fused_app_tokens
			WHERE app_family_id = $1 AND name = $2 FOR UPDATE
		), retained AS (
			UPDATE fused_app_token_history history
			SET status = 'revoked', terminated_at = clock_timestamp(),
			    termination_reason = 'revoked', terminated_by_subject_id = $3,
			    terminated_by_credential_id = $4
			FROM target WHERE history.id = target.id
			RETURNING history.id, history.app_family_id, history.terminated_at
		), deleted AS (
			DELETE FROM fused_app_tokens active USING target
			WHERE active.id = target.id RETURNING active.id
		)
		SELECT retained.id, retained.app_family_id, retained.terminated_at
		FROM retained WHERE EXISTS (SELECT 1 FROM deleted)
	`, appFamilyID, name, nullableUUID(actor.SubjectID), nullableUUID(actor.CredentialID)).Scan(
		&revocation.TokenID, &revocation.AppFamilyID, &revocation.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("revoke app token: %w", err)
	}
	issue := AppTokenIssue{
		ID: revocation.TokenID, AppFamilyID: revocation.AppFamilyID,
		IssuedBySubjectID:    optionalUUIDPointer(actor.SubjectID),
		IssuedByCredentialID: optionalUUIDPointer(actor.CredentialID),
	}
	if err := auditAppTokenMutation(ctx, tx, issue, "app.token.revoke", 0); err != nil {
		return nil, err
	}
	return &revocation, nil
}

func optionalUUIDPointer(value uuid.UUID) *uuid.UUID {
	if value == uuid.Nil {
		return nil
	}
	return &value
}

func auditAppTokenMutation(ctx context.Context, tx pgx.Tx, issue AppTokenIssue, action string, bindingCount int) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, permission,
			resource_type, resource_id, trace_id, outcome, metadata)
		VALUES ($1, $2, $3, $4, 'app', $5, $6, 'succeeded',
		        jsonb_build_object('binding_count', $7::int, 'changed', true))
	`, issue.IssuedBySubjectID, issue.IssuedByCredentialID, action,
		accesscontrol.PermissionAppTokensManage, issue.AppFamilyID,
		trace.SpanFromContext(ctx).SpanContext().TraceID().String(), bindingCount)
	if err != nil {
		return fmt.Errorf("audit app token mutation: %w", err)
	}
	return nil
}

func (s *postgresStore) GetAppTokenBinding(ctx context.Context, tokenID, serviceID uuid.UUID, authName string) (*AppTokenBinding, error) {
	var binding AppTokenBinding
	err := s.db.QueryRow(ctx, `
		SELECT binding.token_id, binding.service_id, binding.auth_name,
		       binding.auth_connection_id, binding.resource_id
		FROM fused_app_token_bindings binding
		JOIN fused_app_tokens token ON token.id = binding.token_id
		WHERE binding.token_id = $1 AND binding.service_id = $2 AND binding.auth_name = $3
		  AND (token.expires_at IS NULL OR token.expires_at > NOW())
	`, tokenID, serviceID, authName).Scan(
		&binding.TokenID, &binding.ServiceID, &binding.AuthName,
		&binding.AuthConnectionID, &binding.ResourceID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get app token binding: %w", err)
	}
	return &binding, nil
}

// ExpireAppTokens deletes credential-bearing rows in bounded keyset batches
// while retaining lifecycle evidence. The database chooses expired rows; Go
// never loads a broad token set merely to filter it by time.
func (s *postgresStore) ExpireAppTokens(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > appFamilyCollectionLimit {
		return 0, errors.New("app token expiry limit is out of range")
	}
	var expiredCount int
	err := s.db.QueryRow(ctx, `
		WITH expired AS (
			SELECT id FROM fused_app_tokens
			WHERE expires_at <= NOW() ORDER BY expires_at, id
			LIMIT $1 FOR UPDATE SKIP LOCKED
		), retained AS (
			UPDATE fused_app_token_history history
			SET status = 'expired', terminated_at = history.expires_at,
			    termination_reason = 'expired'
			FROM expired WHERE history.id = expired.id
		), deleted AS (
			DELETE FROM fused_app_tokens active USING expired
			WHERE active.id = expired.id RETURNING active.id
		)
		SELECT COUNT(*) FROM deleted
	`, limit).Scan(&expiredCount)
	if err != nil {
		return 0, fmt.Errorf("expire app tokens: %w", err)
	}
	return expiredCount, nil
}
