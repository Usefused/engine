package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// UpsertWorkspaceExecutionPolicyOverride writes the service-default row
// (override.ServiceVersionID == nil) or the version-override row
// (override.ServiceVersionID set), never both -- the two tiers are
// distinguished by which partial unique index the ON CONFLICT target matches,
// the same idiom fused_workspace_execution_policies' schema comment
// describes.
func (s *postgresStore) UpsertWorkspaceExecutionPolicyOverride(ctx context.Context, override WorkspaceExecutionPolicyOverride) (*WorkspaceExecutionPolicyOverride, error) {
	if override.ServiceID == uuid.Nil {
		return nil, errors.New("workspace execution policy override requires service_id")
	}
	rateLimit, err := json.Marshal(override.RateLimit)
	if err != nil {
		return nil, fmt.Errorf("marshal rate_limit: %w", err)
	}
	retryConfig, err := json.Marshal(override.RetryConfig)
	if err != nil {
		return nil, fmt.Errorf("marshal retry_config: %w", err)
	}
	pagination, err := json.Marshal(override.Pagination)
	if err != nil {
		return nil, fmt.Errorf("marshal pagination: %w", err)
	}
	incomingWebhookConfig, err := json.Marshal(override.IncomingWebhookConfig)
	if err != nil {
		return nil, fmt.Errorf("marshal incoming_webhook_config: %w", err)
	}

	conflictTarget := "(service_id) WHERE service_version_id IS NULL"
	if override.ServiceVersionID != nil {
		conflictTarget = "(service_version_id) WHERE service_version_id IS NOT NULL"
	}
	query := fmt.Sprintf(`
		INSERT INTO fused_workspace_execution_policies (
			service_id, service_version_id, rate_limit, retry_config,
			pagination, event_extraction_path, incoming_webhook_config, base_url
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT %s DO UPDATE SET
			rate_limit = EXCLUDED.rate_limit,
			retry_config = EXCLUDED.retry_config,
			pagination = EXCLUDED.pagination,
			event_extraction_path = EXCLUDED.event_extraction_path,
			incoming_webhook_config = EXCLUDED.incoming_webhook_config,
			base_url = EXCLUDED.base_url,
			updated_at = NOW()
		RETURNING id, service_id, service_version_id, rate_limit, retry_config,
		          pagination, event_extraction_path, incoming_webhook_config,
		          base_url, created_at, updated_at`, conflictTarget)
	row := s.db.QueryRow(ctx, query, override.ServiceID, override.ServiceVersionID,
		nullableJSON(rateLimit), nullableJSON(retryConfig), nullableJSON(pagination),
		override.EventExtractionPath, nullableJSON(incomingWebhookConfig), override.BaseURL)
	return scanWorkspaceExecutionPolicyOverride(row)
}

// GetEffectiveWorkspaceExecutionPolicyOverride resolves the
// version-override-if-present-else-service-default precedence in SQL: the
// row matching serviceVersionID exactly sorts before the service-default
// (NULL) row, so ORDER BY ... LIMIT 1 picks the more specific override when
// both exist -- the same trick GetEffectiveWorkspaceProfile uses with
// layer DESC.
func (s *postgresStore) GetEffectiveWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*WorkspaceExecutionPolicyOverride, error) {
	query := `
		SELECT id, service_id, service_version_id, rate_limit, retry_config,
		       pagination, event_extraction_path, incoming_webhook_config,
		       base_url, created_at, updated_at
		FROM fused_workspace_execution_policies
		WHERE service_id = $1 AND (service_version_id = $2 OR service_version_id IS NULL)
		ORDER BY service_version_id NULLS LAST
		LIMIT 1`
	override, err := scanWorkspaceExecutionPolicyOverride(s.db.QueryRow(ctx, query, serviceID, serviceVersionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return override, err
}

// ResetWorkspaceExecutionPolicyOverride deletes exactly the tier identified
// by serviceVersionID (nil for the service-default row), leaving the other
// tier -- if any -- untouched.
func (s *postgresStore) ResetWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID uuid.UUID, serviceVersionID *uuid.UUID) error {
	_, err := s.db.Exec(ctx, `
		DELETE FROM fused_workspace_execution_policies
		WHERE service_id = $1 AND service_version_id IS NOT DISTINCT FROM $2`,
		serviceID, serviceVersionID)
	return err
}

// nullableJSON turns json.Marshal(nilPointer)'s "null" into a true SQL NULL
// so an unset field reads back as nil instead of a stored JSON null literal.
func nullableJSON(payload []byte) []byte {
	if len(payload) == 0 || string(payload) == "null" {
		return nil
	}
	return payload
}

// scanWorkspaceExecutionPolicyOverride centralizes positional mapping shared
// by the upsert's RETURNING clause and the effective-row read.
func scanWorkspaceExecutionPolicyOverride(row interface{ Scan(...any) error }) (*WorkspaceExecutionPolicyOverride, error) {
	var override WorkspaceExecutionPolicyOverride
	var rateLimit, retryConfig, pagination, incomingWebhookConfig []byte
	err := row.Scan(&override.ID, &override.ServiceID, &override.ServiceVersionID,
		&rateLimit, &retryConfig, &pagination, &override.EventExtractionPath,
		&incomingWebhookConfig, &override.BaseURL, &override.CreatedAt, &override.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := unmarshalIfPresent(rateLimit, &override.RateLimit); err != nil {
		return nil, fmt.Errorf("decode rate_limit: %w", err)
	}
	if err := unmarshalIfPresent(retryConfig, &override.RetryConfig); err != nil {
		return nil, fmt.Errorf("decode retry_config: %w", err)
	}
	if err := unmarshalIfPresent(pagination, &override.Pagination); err != nil {
		return nil, fmt.Errorf("decode pagination: %w", err)
	}
	if err := unmarshalIfPresent(incomingWebhookConfig, &override.IncomingWebhookConfig); err != nil {
		return nil, fmt.Errorf("decode incoming_webhook_config: %w", err)
	}
	return &override, nil
}

// unmarshalIfPresent leaves dst (a **T field) nil when the stored column was
// SQL NULL, instead of unmarshaling into a zero-value struct -- the
// distinction between "no override for this field" and "override with a
// zero-value config" matters at resolution time.
func unmarshalIfPresent[T any](payload []byte, dst **T) error {
	if len(payload) == 0 {
		return nil
	}
	var v T
	if err := json.Unmarshal(payload, &v); err != nil {
		return err
	}
	*dst = &v
	return nil
}
