package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const connectionResourceColumns = `
	id, connection_id, bucket_id, service_id, provider_resource_id,
	resource_type, display_name, base_url, metadata_json, scopes,
	is_default, is_active, created_at, updated_at`

// UpsertAuthConnectionAndReconcileResources makes callback persistence one
// commit: any ownership, batch-write, normalization, or commit failure leaves
// the previous credential and routing rows intact. Its six set-based statements
// are constant regardless of authoritative resource row count.
func (s *postgresStore) UpsertAuthConnectionAndReconcileResources(ctx context.Context, conn AuthConnection, resources []ConnectionResource) (*AuthConnection, []ConnectionResource, error) {
	// The atomic resource path must enforce the same consent identity as the standalone credential path.
	if err := prepareAuthConnectionForPersistence(&conn); err != nil {
		return nil, nil, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	saved, err := upsertAuthConnectionRow(ctx, tx, conn)
	if err != nil {
		return nil, nil, err
	}
	owned := callbackOwnedResources(saved.ID, resources)
	if err := reconcileConnectionResourcesTx(ctx, tx, saved.ID, owned); err != nil {
		return nil, nil, err
	}
	result, err := listConnectionResources(ctx, tx, saved.ID)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return saved, result, nil
}

// callbackOwnedResources binds only the transaction-created connection ID;
// bucket and service identities remain caller-supplied so ownership drift is
// rejected inside the same transaction instead of silently rewritten.
func callbackOwnedResources(connectionID uuid.UUID, resources []ConnectionResource) []ConnectionResource {
	owned := make([]ConnectionResource, len(resources))
	copy(owned, resources)
	for index := range owned {
		owned[index].ConnectionID = connectionID
	}
	return owned
}

// ReconcileConnectionResources makes one discovery response authoritative in
// a transaction so callers never observe a half-refreshed tenant list.
func (s *postgresStore) ReconcileConnectionResources(ctx context.Context, connectionID uuid.UUID, resources []ConnectionResource) ([]ConnectionResource, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if err := reconcileConnectionResourcesTx(ctx, tx, connectionID, resources); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.ListConnectionResources(ctx, connectionID)
}

// reconcileConnectionResourcesTx is the shared set-based reconciliation used
// by manual discovery and callback credential transactions.
func reconcileConnectionResourcesTx(ctx context.Context, tx pgx.Tx, connectionID uuid.UUID, resources []ConnectionResource) error {
	if err := verifyResourceOwnership(ctx, tx, connectionID, resources); err != nil {
		return err
	}
	if err := deactivateMissingResources(ctx, tx, connectionID, resources); err != nil {
		return err
	}
	if err := upsertConnectionResources(ctx, tx, connectionID, resources); err != nil {
		return err
	}
	return normalizeConnectionResourceDefault(ctx, tx, connectionID)
}

// verifyResourceOwnership rejects mixed batches before SQL can persist routing
// context under a connection that belongs to another bucket or service.
func verifyResourceOwnership(ctx context.Context, tx pgx.Tx, connectionID uuid.UUID, resources []ConnectionResource) error {
	var bucketID, serviceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT bucket_id, service_id FROM fused_auth_connections WHERE id = $1`, connectionID).Scan(&bucketID, &serviceID); err != nil {
		return err
	}
	for _, resource := range resources {
		if resource.ConnectionID != connectionID || resource.BucketID != bucketID || resource.ServiceID != serviceID {
			return errors.New("connection resource ownership mismatch")
		}
	}
	return nil
}

// deactivateMissingResources uses a tuple array in SQL so authoritative
// discovery reconciliation does not load existing rows into Go for filtering.
func deactivateMissingResources(ctx context.Context, tx pgx.Tx, connectionID uuid.UUID, resources []ConnectionResource) error {
	resourceTypes := make([]string, 0, len(resources))
	providerIDs := make([]string, 0, len(resources))
	for _, resource := range resources {
		resourceTypes = append(resourceTypes, resource.ResourceType)
		providerIDs = append(providerIDs, resource.ProviderResourceID)
	}
	_, err := tx.Exec(ctx, `
		UPDATE fused_connection_resources
		SET is_active = false, is_default = false, updated_at = NOW()
		WHERE connection_id = $1
		AND NOT ((resource_type, provider_resource_id) IN (
			SELECT * FROM unnest($2::text[], $3::text[])
		))`, connectionID, resourceTypes, providerIDs)
	return err
}

// upsertConnectionResources writes the bounded discovery result as one batch;
// defaults are normalized only after every current resource is active again.
func upsertConnectionResources(ctx context.Context, tx pgx.Tx, connectionID uuid.UUID, resources []ConnectionResource) error {
	type resourceRow struct {
		BucketID           uuid.UUID       `json:"bucket_id"`
		ServiceID          uuid.UUID       `json:"service_id"`
		ProviderResourceID string          `json:"provider_resource_id"`
		ResourceType       string          `json:"resource_type"`
		DisplayName        string          `json:"display_name"`
		BaseURL            string          `json:"base_url"`
		MetadataJSON       json.RawMessage `json:"metadata_json"`
		Scopes             []string        `json:"scopes"`
	}
	rows := make([]resourceRow, 0, len(resources))
	for _, resource := range resources {
		metadata := resource.MetadataJSON
		if !json.Valid(metadata) {
			metadata = []byte(`{}`)
		}
		rows = append(rows, resourceRow{
			BucketID: resource.BucketID, ServiceID: resource.ServiceID,
			ProviderResourceID: resource.ProviderResourceID, ResourceType: resource.ResourceType,
			DisplayName: resource.DisplayName, BaseURL: resource.BaseURL,
			MetadataJSON: metadata, Scopes: nonNilStrings(resource.Scopes),
		})
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO fused_connection_resources (
			connection_id, bucket_id, service_id, provider_resource_id,
			resource_type, display_name, base_url, metadata_json, scopes, is_active
		)
		SELECT $1, x.bucket_id, x.service_id, x.provider_resource_id,
		       x.resource_type, x.display_name, x.base_url, x.metadata_json, x.scopes, true
		FROM jsonb_to_recordset($2::jsonb) AS x(
			bucket_id uuid, service_id uuid, provider_resource_id text,
			resource_type text, display_name text, base_url text,
			metadata_json jsonb, scopes text[]
		)
		ON CONFLICT (connection_id, resource_type, provider_resource_id) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			base_url = EXCLUDED.base_url,
			metadata_json = EXCLUDED.metadata_json,
			scopes = EXCLUDED.scopes,
			is_active = true,
			updated_at = NOW()`, connectionID, payload)
	return err
}

// normalizeConnectionResourceDefault preserves a valid explicit default and
// only auto-selects when one active resource makes the choice unambiguous.
func normalizeConnectionResourceDefault(ctx context.Context, tx pgx.Tx, connectionID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		WITH state AS (
			SELECT COUNT(*) FILTER (WHERE is_active) AS active_count,
			       COUNT(*) FILTER (WHERE is_active AND is_default) AS default_count
			FROM fused_connection_resources WHERE connection_id = $1
		)
		UPDATE fused_connection_resources r
		SET is_default = true, updated_at = NOW()
		FROM state
		WHERE r.connection_id = $1 AND r.is_active
		  AND state.active_count = 1 AND state.default_count = 0`, connectionID)
	return err
}

// GetConnectionResourceForExecution resolves either an exact internal ID or
// the sole/default resource in SQL and returns the active count for ambiguity.
func (s *postgresStore) GetConnectionResourceForExecution(ctx context.Context, connectionID uuid.UUID, resourceID *uuid.UUID) (*ConnectionResource, int, error) {
	var activeCount int
	query := `
		WITH candidates AS (
			SELECT *, COUNT(*) OVER () AS active_count
			FROM fused_connection_resources
			WHERE connection_id = $1 AND is_active
		), selected AS (
			SELECT * FROM candidates
			WHERE ($2::uuid IS NOT NULL AND id = $2)
			   OR ($2::uuid IS NULL AND (is_default OR active_count = 1))
			ORDER BY is_default DESC LIMIT 1
		)
		SELECT ` + connectionResourceColumns + `, active_count FROM selected`
	resource, err := scanConnectionResourceWithCount(s.db.QueryRow(ctx, query, connectionID, nullableUUIDPtr(resourceID)), &activeCount)
	if errors.Is(err, pgx.ErrNoRows) {
		if countErr := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM fused_connection_resources WHERE connection_id = $1 AND is_active`, connectionID).Scan(&activeCount); countErr != nil {
			return nil, 0, countErr
		}
		return nil, activeCount, nil
	}
	return resource, activeCount, err
}

// ListConnectionResources returns active resources already filtered and
// ordered by SQL for UI, CLI, and generated SDK inspection.
func (s *postgresStore) ListConnectionResources(ctx context.Context, connectionID uuid.UUID) ([]ConnectionResource, error) {
	return listConnectionResources(ctx, s.db, connectionID)
}

type connectionResourceQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// listConnectionResources projects active rows through either the pool or the
// still-open callback transaction, preventing a pre-commit visibility gap.
func listConnectionResources(ctx context.Context, querier connectionResourceQuerier, connectionID uuid.UUID) ([]ConnectionResource, error) {
	rows, err := querier.Query(ctx, `SELECT `+connectionResourceColumns+` FROM fused_connection_resources WHERE connection_id = $1 AND is_active ORDER BY is_default DESC, display_name, id`, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	resources := []ConnectionResource{}
	for rows.Next() {
		resource, err := scanConnectionResource(rows)
		if err != nil {
			return nil, err
		}
		resources = append(resources, *resource)
	}
	return resources, rows.Err()
}

// SetDefaultConnectionResource changes the default atomically and scopes the
// requested resource to its connection to prevent cross-user selection.
func (s *postgresStore) SetDefaultConnectionResource(ctx context.Context, connectionID, resourceID uuid.UUID) (*ConnectionResource, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE fused_connection_resources SET is_default = false, updated_at = NOW() WHERE connection_id = $1 AND is_default`, connectionID); err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx, `UPDATE fused_connection_resources SET is_default = true, updated_at = NOW() WHERE connection_id = $1 AND id = $2 AND is_active RETURNING `+connectionResourceColumns, connectionID, resourceID)
	resource, err := scanConnectionResource(row)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return resource, nil
}

// scanConnectionResource centralizes the positional mapping shared by query
// and mutation paths so adding metadata cannot silently diverge between them.
func scanConnectionResource(row interface{ Scan(...any) error }) (*ConnectionResource, error) {
	var resource ConnectionResource
	err := row.Scan(
		&resource.ID, &resource.ConnectionID, &resource.BucketID, &resource.ServiceID,
		&resource.ProviderResourceID, &resource.ResourceType, &resource.DisplayName,
		&resource.BaseURL, &resource.MetadataJSON, &resource.Scopes, &resource.IsDefault,
		&resource.IsActive, &resource.CreatedAt, &resource.UpdatedAt,
	)
	return &resource, err
}

// scanConnectionResourceWithCount extends the shared row mapping with the
// window-count used to distinguish missing from ambiguous selection.
func scanConnectionResourceWithCount(row interface{ Scan(...any) error }, count *int) (*ConnectionResource, error) {
	var resource ConnectionResource
	err := row.Scan(
		&resource.ID, &resource.ConnectionID, &resource.BucketID, &resource.ServiceID,
		&resource.ProviderResourceID, &resource.ResourceType, &resource.DisplayName,
		&resource.BaseURL, &resource.MetadataJSON, &resource.Scopes, &resource.IsDefault,
		&resource.IsActive, &resource.CreatedAt, &resource.UpdatedAt, count,
	)
	return &resource, err
}
