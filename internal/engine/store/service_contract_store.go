package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrServiceContractSnapshotNotFound = errors.New("service contract snapshot not found")
var ErrServiceContractEndpointNotFound = errors.New("service contract endpoint not found")

type ServiceContractSnapshot struct {
	ID               uuid.UUID
	ServiceID        uuid.UUID
	ServiceVersionID uuid.UUID
	Version          string
	Revision         int
	SourceHash       string
	ContractHash     string
	Status           string
	ServiceMetadata  fusedobject.ServiceMetadata
	Endpoints        []fusedobject.Endpoint
	Webhooks         []fusedobject.Webhook
	FetchedAt        time.Time
	RefreshedAt      time.Time
	LastRefreshError string
}

// ServiceContractEndpointSelection is the database-facing portion of one app
// selection. SelectionIndex preserves the app's declared order without making
// the store understand SDK/MCP wire models.
type ServiceContractEndpointSelection struct {
	SelectionIndex   int         `json:"selection_index"`
	ServiceID        uuid.UUID   `json:"service_id"`
	ServiceVersionID uuid.UUID   `json:"service_version_id"`
	SelectAll        bool        `json:"select_all"`
	EndpointIDs      []uuid.UUID `json:"endpoint_ids"`
}

// ServiceContractEndpointMatch contains only rows already intersected by the
// app selection and optional token allowlist in PostgreSQL.
type ServiceContractEndpointMatch struct {
	SelectionIndex int
	Endpoint       fusedobject.Endpoint
}

type ServiceContractSnapshotStore interface {
	UpsertServiceContractSnapshot(ctx context.Context, snapshot ServiceContractSnapshot) (*ServiceContractSnapshot, error)
	GetServiceContractMetadata(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*fusedobject.ServiceMetadata, error)
	GetServiceContractEndpointByName(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointName string) (*fusedobject.Endpoint, error)
	ListServiceContractEndpointsByNames(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error)
	ListServiceContractEndpointsForSelections(ctx context.Context, selections []ServiceContractEndpointSelection, endpointNames []string) ([]ServiceContractEndpointMatch, error)
	ListServiceContractEndpointsByIDs(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointIDs []uuid.UUID) ([]fusedobject.Endpoint, error)
	ListServiceContractOperations(ctx context.Context, serviceID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error)
}

func (s *postgresStore) UpsertServiceContractSnapshot(ctx context.Context, snapshot ServiceContractSnapshot) (*ServiceContractSnapshot, error) {
	prepared, metadataJSON, err := prepareServiceContractSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert service contract snapshot: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	saved, err := replaceServiceContractSnapshotRows(ctx, tx, prepared, metadataJSON)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("upsert service contract snapshot: commit: %w", err)
	}
	return saved, nil
}

func prepareServiceContractSnapshot(snapshot ServiceContractSnapshot) (ServiceContractSnapshot, []byte, error) {
	if err := validateServiceContractSnapshot(snapshot); err != nil {
		return snapshot, nil, err
	}
	if snapshot.Status == "" {
		snapshot.Status = "active"
	}
	if snapshot.ContractHash == "" {
		hash, err := serviceContractHash(snapshot)
		if err != nil {
			return snapshot, nil, err
		}
		snapshot.ContractHash = hash
	}
	metadataJSON, err := json.Marshal(snapshot.ServiceMetadata)
	if err != nil {
		return snapshot, nil, fmt.Errorf("marshal service metadata: %w", err)
	}
	return snapshot, metadataJSON, nil
}

func replaceServiceContractSnapshotRows(ctx context.Context, tx pgx.Tx, snapshot ServiceContractSnapshot, metadataJSON []byte) (*ServiceContractSnapshot, error) {
	var snapshotID uuid.UUID
	var fetchedAt, refreshedAt time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO fused_service_contract_snapshots (
			service_id, service_version_id, version, revision, source_hash,
			contract_hash, contract_status, service_metadata, last_refresh_error
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (service_version_id) DO UPDATE SET
			service_id = EXCLUDED.service_id,
			version = EXCLUDED.version,
			revision = EXCLUDED.revision,
			source_hash = EXCLUDED.source_hash,
			contract_hash = EXCLUDED.contract_hash,
			contract_status = EXCLUDED.contract_status,
			service_metadata = EXCLUDED.service_metadata,
			refreshed_at = NOW(),
			last_refresh_error = EXCLUDED.last_refresh_error
		RETURNING id, fetched_at, refreshed_at`,
		snapshot.ServiceID, snapshot.ServiceVersionID, snapshot.Version, snapshot.Revision,
		snapshot.SourceHash, snapshot.ContractHash, snapshot.Status, metadataJSON, snapshot.LastRefreshError,
	).Scan(&snapshotID, &fetchedAt, &refreshedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert service contract snapshot: %w", err)
	}
	if err := replaceServiceContractEndpoints(ctx, tx, snapshotID, snapshot.Endpoints); err != nil {
		return nil, err
	}
	if err := replaceServiceContractWebhooks(ctx, tx, snapshotID, snapshot.Webhooks); err != nil {
		return nil, err
	}

	snapshot.ID = snapshotID
	snapshot.FetchedAt = fetchedAt
	snapshot.RefreshedAt = refreshedAt
	return &snapshot, nil
}

func validateServiceContractSnapshot(snapshot ServiceContractSnapshot) error {
	if snapshot.ServiceID == uuid.Nil {
		return errors.New("service contract snapshot requires service_id")
	}
	if snapshot.ServiceVersionID == uuid.Nil {
		return errors.New("service contract snapshot requires service_version_id")
	}
	if snapshot.Version == "" {
		return errors.New("service contract snapshot requires version")
	}
	return nil
}

func replaceServiceContractEndpoints(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, endpoints []fusedobject.Endpoint) error {
	if _, err := tx.Exec(ctx, `DELETE FROM fused_service_contract_endpoints WHERE snapshot_id = $1`, snapshotID); err != nil {
		return fmt.Errorf("replace service contract endpoints: delete: %w", err)
	}
	for _, endpoint := range endpoints {
		if endpoint.ID == uuid.Nil {
			return fmt.Errorf("replace service contract endpoints: endpoint %q has no id", endpoint.Name)
		}
		payload, err := json.Marshal(endpoint)
		if err != nil {
			return fmt.Errorf("replace service contract endpoints: marshal %s: %w", endpoint.Name, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO fused_service_contract_endpoints (
				snapshot_id, endpoint_id, name, method, path, normalized_path, operation_json
			)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			snapshotID, endpoint.ID, endpoint.Name, endpoint.Method, endpoint.Path, endpoint.NormalizedPath, payload,
		); err != nil {
			return fmt.Errorf("replace service contract endpoints: insert %s: %w", endpoint.Name, err)
		}
	}
	return nil
}

func replaceServiceContractWebhooks(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, webhooks []fusedobject.Webhook) error {
	if _, err := tx.Exec(ctx, `DELETE FROM fused_service_contract_webhooks WHERE snapshot_id = $1`, snapshotID); err != nil {
		return fmt.Errorf("replace service contract webhooks: delete: %w", err)
	}
	for _, webhook := range webhooks {
		if webhook.ID == uuid.Nil {
			return fmt.Errorf("replace service contract webhooks: webhook %q has no id", webhook.Name)
		}
		payload, err := json.Marshal(webhook)
		if err != nil {
			return fmt.Errorf("replace service contract webhooks: marshal %s: %w", webhook.Name, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO fused_service_contract_webhooks (
				snapshot_id, webhook_id, name, method, webhook_json
			)
			VALUES ($1,$2,$3,$4,$5)`,
			snapshotID, webhook.ID, webhook.Name, webhook.Method, payload,
		); err != nil {
			return fmt.Errorf("replace service contract webhooks: insert %s: %w", webhook.Name, err)
		}
	}
	return nil
}

func (s *postgresStore) GetServiceContractMetadata(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*fusedobject.ServiceMetadata, error) {
	var payload []byte
	err := s.db.QueryRow(ctx, `
		SELECT service_metadata
		FROM fused_service_contract_snapshots
		WHERE service_id = $1 AND service_version_id = $2`,
		serviceID, serviceVersionID,
	).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrServiceContractSnapshotNotFound
	}
	if err != nil {
		return nil, err
	}
	var metadata fusedobject.ServiceMetadata
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return nil, fmt.Errorf("decode service contract metadata: %w", err)
	}
	return &metadata, nil
}

func (s *postgresStore) GetServiceContractEndpointByName(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointName string) (*fusedobject.Endpoint, error) {
	var payload []byte
	err := s.db.QueryRow(ctx, `
		SELECT endpoints.operation_json
		FROM fused_service_contract_snapshots snapshots
		LEFT JOIN fused_service_contract_endpoints endpoints
		  ON endpoints.snapshot_id = snapshots.id
		 AND endpoints.name = $3
		WHERE snapshots.service_id = $1
		  AND snapshots.service_version_id = $2`,
		serviceID, serviceVersionID, endpointName,
	).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrServiceContractSnapshotNotFound
	}
	if err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, ErrServiceContractEndpointNotFound
	}
	var endpoint fusedobject.Endpoint
	if err := json.Unmarshal(payload, &endpoint); err != nil {
		return nil, fmt.Errorf("decode service contract endpoint: %w", err)
	}
	return &endpoint, nil
}

func (s *postgresStore) ListServiceContractEndpointsByNames(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error) {
	snapshotID, err := s.getServiceContractSnapshotID(ctx, serviceID, serviceVersionID)
	if err != nil || len(endpointNames) == 0 {
		return nil, err
	}
	return s.listServiceContractEndpoints(ctx, `
		SELECT operation_json
		FROM fused_service_contract_endpoints
		WHERE snapshot_id = $1
		  AND name = ANY($2)
		ORDER BY name`,
		snapshotID, endpointNames,
	)
}

func (s *postgresStore) ListServiceContractEndpointsForSelections(ctx context.Context, selections []ServiceContractEndpointSelection, endpointNames []string) ([]ServiceContractEndpointMatch, error) {
	if len(selections) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(selections)
	if err != nil {
		return nil, fmt.Errorf("encode service contract endpoint selections: %w", err)
	}
	rows, err := s.db.Query(ctx, `
		WITH requested AS (
			SELECT (item->>'selection_index')::integer AS selection_index,
			       (item->>'service_id')::uuid AS service_id,
			       (item->>'service_version_id')::uuid AS service_version_id,
			       COALESCE((item->>'select_all')::boolean, false) AS select_all,
			       CASE WHEN jsonb_typeof(item->'endpoint_ids') = 'array'
			            THEN item->'endpoint_ids' ELSE '[]'::jsonb END AS endpoint_ids
			FROM jsonb_array_elements($1::jsonb) AS item
		), resolved AS (
			SELECT requested.*, snapshots.id AS snapshot_id
			FROM requested
			LEFT JOIN fused_service_contract_snapshots snapshots
			  ON snapshots.service_id = requested.service_id
			 AND snapshots.service_version_id = requested.service_version_id
		)
		SELECT resolved.selection_index, resolved.snapshot_id, endpoints.operation_json
		FROM resolved
		LEFT JOIN fused_service_contract_endpoints endpoints
		  ON endpoints.snapshot_id = resolved.snapshot_id
		 AND (COALESCE(cardinality($2::text[]), 0) = 0 OR endpoints.name = ANY($2::text[]))
		 AND (
		   resolved.select_all
		   OR EXISTS (
		     SELECT 1
		     FROM jsonb_array_elements_text(resolved.endpoint_ids) allowed(endpoint_id)
		     WHERE allowed.endpoint_id::uuid = endpoints.endpoint_id
		   )
		 )
		ORDER BY resolved.selection_index, endpoints.name`, payload, endpointNames)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanServiceContractEndpointMatches(rows)
}

func scanServiceContractEndpointMatches(rows pgx.Rows) ([]ServiceContractEndpointMatch, error) {
	var matches []ServiceContractEndpointMatch
	for rows.Next() {
		var selectionIndex int
		var snapshotID *uuid.UUID
		var operationJSON []byte
		if err := rows.Scan(&selectionIndex, &snapshotID, &operationJSON); err != nil {
			return nil, err
		}
		if snapshotID == nil {
			return nil, ErrServiceContractSnapshotNotFound
		}
		if operationJSON == nil {
			continue
		}
		var endpoint fusedobject.Endpoint
		if err := json.Unmarshal(operationJSON, &endpoint); err != nil {
			return nil, fmt.Errorf("decode selected service contract operation: %w", err)
		}
		matches = append(matches, ServiceContractEndpointMatch{SelectionIndex: selectionIndex, Endpoint: endpoint})
	}
	return matches, rows.Err()
}

func (s *postgresStore) ListServiceContractEndpointsByIDs(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointIDs []uuid.UUID) ([]fusedobject.Endpoint, error) {
	snapshotID, err := s.getServiceContractSnapshotID(ctx, serviceID, serviceVersionID)
	if err != nil || len(endpointIDs) == 0 {
		return nil, err
	}
	return s.listServiceContractEndpoints(ctx, `
		SELECT operation_json
		FROM fused_service_contract_endpoints
		WHERE snapshot_id = $1
		  AND endpoint_id = ANY($2)
		ORDER BY name`,
		snapshotID, endpointIDs,
	)
}

func (s *postgresStore) ListServiceContractOperations(ctx context.Context, serviceID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error) {
	snapshotID, err := s.getServiceContractSnapshotID(ctx, serviceID, serviceVersionID)
	if err != nil {
		return nil, err
	}
	return s.listServiceContractEndpoints(ctx, `
		SELECT operation_json
		FROM fused_service_contract_endpoints
		WHERE snapshot_id = $1
		ORDER BY name`,
		snapshotID,
	)
}

func (s *postgresStore) getServiceContractSnapshotID(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (uuid.UUID, error) {
	var snapshotID uuid.UUID
	err := s.db.QueryRow(ctx, `
		SELECT id
		FROM fused_service_contract_snapshots
		WHERE service_id = $1 AND service_version_id = $2`,
		serviceID, serviceVersionID,
	).Scan(&snapshotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrServiceContractSnapshotNotFound
	}
	return snapshotID, err
}

func (s *postgresStore) listServiceContractEndpoints(ctx context.Context, query string, args ...any) ([]fusedobject.Endpoint, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fusedobject.Endpoint
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var endpoint fusedobject.Endpoint
		if err := json.Unmarshal(payload, &endpoint); err != nil {
			return nil, fmt.Errorf("decode service contract operation: %w", err)
		}
		out = append(out, endpoint)
	}
	return out, rows.Err()
}

func serviceContractHash(snapshot ServiceContractSnapshot) (string, error) {
	stable := struct {
		ServiceMetadata fusedobject.ServiceMetadata `json:"service_metadata"`
		Endpoints       []fusedobject.Endpoint      `json:"endpoints"`
		Webhooks        []fusedobject.Webhook       `json:"webhooks"`
	}{
		ServiceMetadata: snapshot.ServiceMetadata,
		Endpoints:       append([]fusedobject.Endpoint(nil), snapshot.Endpoints...),
		Webhooks:        append([]fusedobject.Webhook(nil), snapshot.Webhooks...),
	}
	sort.Slice(stable.Endpoints, func(i, j int) bool {
		return stable.Endpoints[i].ID.String() < stable.Endpoints[j].ID.String()
	})
	sort.Slice(stable.Webhooks, func(i, j int) bool {
		return stable.Webhooks[i].ID.String() < stable.Webhooks[j].ID.String()
	})
	payload, err := json.Marshal(stable)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
