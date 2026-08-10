package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Usefused/engine/internal/shared/paginationpolicy"
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
			timeout_ms, pagination, event_extraction_path, incoming_webhook_config, base_url
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT %s DO UPDATE SET
			rate_limit = EXCLUDED.rate_limit,
			retry_config = EXCLUDED.retry_config,
			timeout_ms = EXCLUDED.timeout_ms,
			pagination = EXCLUDED.pagination,
			event_extraction_path = EXCLUDED.event_extraction_path,
			incoming_webhook_config = EXCLUDED.incoming_webhook_config,
			base_url = EXCLUDED.base_url,
			updated_at = NOW()
		RETURNING id, service_id, service_version_id, rate_limit, retry_config,
		          timeout_ms, pagination, event_extraction_path, incoming_webhook_config,
		          base_url, created_at, updated_at`, conflictTarget)
	row := s.db.QueryRow(ctx, query, override.ServiceID, override.ServiceVersionID,
		nullableJSON(rateLimit), nullableJSON(retryConfig), override.TimeoutMs, nullableJSON(pagination),
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
		SELECT id, service_id, service_version_id, rate_limit, retry_config, timeout_ms,
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

func (s *postgresStore) GetEffectiveWorkspaceExecutionPolicyOverrides(ctx context.Context, refs []WorkspaceExecutionPolicyRef) (map[WorkspaceExecutionPolicyRef]*WorkspaceExecutionPolicyOverride, error) {
	result := make(map[WorkspaceExecutionPolicyRef]*WorkspaceExecutionPolicyOverride, len(refs))
	if len(refs) == 0 {
		return result, nil
	}
	serviceIDs, versionIDs := workspaceExecutionPolicyRefArrays(refs)
	rows, err := s.db.Query(ctx, `
		SELECT input.service_id, input.service_version_id,
		       policy.id, policy.service_id, policy.service_version_id,
		       policy.rate_limit, policy.retry_config, policy.timeout_ms, policy.pagination,
		       policy.event_extraction_path, policy.incoming_webhook_config,
		       policy.base_url, policy.created_at, policy.updated_at
		FROM unnest($1::uuid[], $2::uuid[]) AS input(service_id, service_version_id)
		JOIN LATERAL (
			SELECT * FROM fused_workspace_execution_policies candidate
			WHERE candidate.service_id = input.service_id
			  AND (candidate.service_version_id = input.service_version_id OR candidate.service_version_id IS NULL)
			ORDER BY candidate.service_version_id NULLS LAST LIMIT 1
		) policy ON TRUE`, serviceIDs, versionIDs)
	if err != nil {
		return nil, fmt.Errorf("GetEffectiveWorkspaceExecutionPolicyOverrides: query: %w", err)
	}
	defer rows.Close()
	return collectWorkspaceExecutionPolicyOverrides(rows, result)
}

func (s *postgresStore) GetWorkspaceExecutionPolicyOverrides(ctx context.Context, refs []WorkspaceExecutionPolicyRef) (map[WorkspaceExecutionPolicyRef]*WorkspaceExecutionPolicyOverride, error) {
	result := make(map[WorkspaceExecutionPolicyRef]*WorkspaceExecutionPolicyOverride, len(refs))
	if len(refs) == 0 {
		return result, nil
	}
	serviceIDs, versionIDs := workspaceExecutionPolicyRefArrays(refs)
	rows, err := s.db.Query(ctx, `
		SELECT input.service_id, input.service_version_id,
		       policy.id, policy.service_id, policy.service_version_id,
		       policy.rate_limit, policy.retry_config, policy.timeout_ms, policy.pagination,
		       policy.event_extraction_path, policy.incoming_webhook_config,
		       policy.base_url, policy.created_at, policy.updated_at
		FROM unnest($1::uuid[], $2::uuid[]) AS input(service_id, service_version_id)
		JOIN fused_workspace_execution_policies policy
		  ON policy.service_id = input.service_id
		 AND policy.service_version_id IS NOT DISTINCT FROM
		     NULLIF(input.service_version_id, '00000000-0000-0000-0000-000000000000'::uuid)`, serviceIDs, versionIDs)
	if err != nil {
		return nil, fmt.Errorf("GetWorkspaceExecutionPolicyOverrides: query: %w", err)
	}
	defer rows.Close()
	return collectWorkspaceExecutionPolicyOverrides(rows, result)
}

func workspaceExecutionPolicyRefArrays(refs []WorkspaceExecutionPolicyRef) ([]uuid.UUID, []uuid.UUID) {
	serviceIDs := make([]uuid.UUID, len(refs))
	versionIDs := make([]uuid.UUID, len(refs))
	for i, ref := range refs {
		serviceIDs[i], versionIDs[i] = ref.ServiceID, ref.ServiceVersionID
	}
	return serviceIDs, versionIDs
}

type workspaceExecutionPolicyRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func collectWorkspaceExecutionPolicyOverrides(rows workspaceExecutionPolicyRows, result map[WorkspaceExecutionPolicyRef]*WorkspaceExecutionPolicyOverride) (map[WorkspaceExecutionPolicyRef]*WorkspaceExecutionPolicyOverride, error) {
	for rows.Next() {
		var ref WorkspaceExecutionPolicyRef
		var override WorkspaceExecutionPolicyOverride
		var rateLimit, retryConfig, pagination, incomingWebhookConfig []byte
		if err := rows.Scan(&ref.ServiceID, &ref.ServiceVersionID, &override.ID, &override.ServiceID, &override.ServiceVersionID,
			&rateLimit, &retryConfig, &override.TimeoutMs, &pagination, &override.EventExtractionPath,
			&incomingWebhookConfig, &override.BaseURL, &override.CreatedAt, &override.UpdatedAt); err != nil {
			return nil, err
		}
		if err := decodeWorkspaceExecutionPolicyJSON(&override, rateLimit, retryConfig, pagination, incomingWebhookConfig); err != nil {
			return nil, err
		}
		result[ref] = &override
	}
	return result, rows.Err()
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
		&rateLimit, &retryConfig, &override.TimeoutMs, &pagination, &override.EventExtractionPath,
		&incomingWebhookConfig, &override.BaseURL, &override.CreatedAt, &override.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := decodeWorkspaceExecutionPolicyJSON(&override, rateLimit, retryConfig, pagination, incomingWebhookConfig); err != nil {
		return nil, err
	}
	return &override, nil
}

func decodeWorkspaceExecutionPolicyJSON(override *WorkspaceExecutionPolicyOverride, rateLimit, retryConfig, pagination, incomingWebhookConfig []byte) error {
	if err := unmarshalIfPresent(rateLimit, &override.RateLimit); err != nil {
		return fmt.Errorf("decode rate_limit: %w", err)
	}
	if err := unmarshalIfPresent(retryConfig, &override.RetryConfig); err != nil {
		return fmt.Errorf("decode retry_config: %w", err)
	}
	if err := unmarshalIfPresent(pagination, &override.Pagination); err != nil {
		return fmt.Errorf("decode pagination: %w", err)
	}
	if override.Pagination != nil {
		if err := paginationpolicy.Validate((*paginationpolicy.Config)(override.Pagination)); err != nil {
			return fmt.Errorf("decode pagination: %w", err)
		}
	}
	if err := unmarshalIfPresent(incomingWebhookConfig, &override.IncomingWebhookConfig); err != nil {
		return fmt.Errorf("decode incoming_webhook_config: %w", err)
	}
	return nil
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
