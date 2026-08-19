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

// A row bound keeps each JSONB statement predictable while avoiding one
// database round trip per operation. Snapshot writes therefore use exactly
// 3 + ceil(endpoints/100) + ceil(webhooks/100) DML statements.
const serviceContractSnapshotWriteBatchSize = 100

type ServiceContractSnapshot struct {
	fusedobject.ExecutionContractEnvelope
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

type serviceContractHashInput struct {
	ContractVersion      int                         `json:"contract_version"`
	RequiredCapabilities []string                    `json:"required_capabilities"`
	ServiceMetadata      fusedobject.ServiceMetadata `json:"service_metadata"`
	Endpoints            []fusedobject.Endpoint      `json:"endpoints"`
	Webhooks             []fusedobject.Webhook       `json:"webhooks"`
}

type serviceContractEndpointRow struct {
	EndpointID     uuid.UUID       `json:"endpoint_id"`
	Name           string          `json:"name"`
	Method         string          `json:"method"`
	Path           string          `json:"path"`
	NormalizedPath string          `json:"normalized_path"`
	OperationJSON  json.RawMessage `json:"operation_json"`
}

type serviceContractWebhookRow struct {
	WebhookID   uuid.UUID       `json:"webhook_id"`
	Name        string          `json:"name"`
	Method      string          `json:"method"`
	WebhookJSON json.RawMessage `json:"webhook_json"`
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
	OperationNames   []string    `json:"operation_names,omitempty"`
	// EndpointNames narrows this selection independently from every other
	// selection in the batch. Unified bindings need this because two services
	// may expose the same operationId without admitting either service's other
	// selected operations into the result set.
	EndpointNames []string `json:"endpoint_names,omitempty"`
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

// UpsertServiceContractSnapshot prepares all rows before opening the transaction
// so validation cannot leave a partially replaced executable snapshot.
func (s *postgresStore) UpsertServiceContractSnapshot(ctx context.Context, snapshot ServiceContractSnapshot) (*ServiceContractSnapshot, error) {
	prepared, metadataJSON, err := prepareServiceContractSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	endpointRows, err := prepareServiceContractEndpointRows(prepared.Endpoints)
	if err != nil {
		return nil, err
	}
	webhookRows, err := prepareServiceContractWebhookRows(prepared.Webhooks)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert service contract snapshot: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	saved, err := replaceServiceContractSnapshotRows(ctx, tx, prepared, metadataJSON, endpointRows, webhookRows)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("upsert service contract snapshot: commit: %w", err)
	}
	return saved, nil
}

func prepareServiceContractSnapshot(snapshot ServiceContractSnapshot) (ServiceContractSnapshot, []byte, error) {
	envelope, err := fusedobject.CanonicalExecutionContractEnvelope(snapshot.ExecutionContractEnvelope)
	if err != nil {
		return snapshot, nil, fmt.Errorf("service contract snapshot: %w", err)
	}
	snapshot.ExecutionContractEnvelope = envelope
	if err := validateServiceContractSnapshot(snapshot); err != nil {
		return snapshot, nil, err
	}
	if snapshot.Status == "" {
		snapshot.Status = "active"
	}
	// Recompute locally on every write so a caller cannot carry forward a hash
	// that predates newly negotiated execution semantics.
	hash, err := serviceContractHash(snapshot)
	if err != nil {
		return snapshot, nil, err
	}
	snapshot.ContractHash = hash
	metadataJSON, err := json.Marshal(snapshot.ServiceMetadata)
	if err != nil {
		return snapshot, nil, fmt.Errorf("marshal service metadata: %w", err)
	}
	return snapshot, metadataJSON, nil
}

// replaceServiceContractSnapshotRows replaces metadata and child surfaces in one
// transaction so readers never observe operations from a different revision.
func replaceServiceContractSnapshotRows(
	ctx context.Context,
	tx pgx.Tx,
	snapshot ServiceContractSnapshot,
	metadataJSON []byte,
	endpointRows []serviceContractEndpointRow,
	webhookRows []serviceContractWebhookRow,
) (*ServiceContractSnapshot, error) {
	var snapshotID uuid.UUID
	var fetchedAt, refreshedAt time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO fused_service_contract_snapshots (
			service_id, service_version_id, version, contract_version, required_capabilities, revision, source_hash,
			contract_hash, contract_status, service_metadata, last_refresh_error
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (service_version_id) DO UPDATE SET
			service_id = EXCLUDED.service_id,
			version = EXCLUDED.version,
			contract_version = EXCLUDED.contract_version,
			required_capabilities = EXCLUDED.required_capabilities,
			revision = EXCLUDED.revision,
			source_hash = EXCLUDED.source_hash,
			contract_hash = EXCLUDED.contract_hash,
			contract_status = EXCLUDED.contract_status,
			service_metadata = EXCLUDED.service_metadata,
			refreshed_at = NOW(),
			last_refresh_error = EXCLUDED.last_refresh_error
		RETURNING id, contract_version, required_capabilities, fetched_at, refreshed_at`,
		snapshot.ServiceID, snapshot.ServiceVersionID, snapshot.Version,
		snapshot.ContractVersion, snapshot.RequiredCapabilities, snapshot.Revision,
		snapshot.SourceHash, snapshot.ContractHash, snapshot.Status, metadataJSON, snapshot.LastRefreshError,
	).Scan(&snapshotID, &snapshot.ContractVersion, &snapshot.RequiredCapabilities, &fetchedAt, &refreshedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert service contract snapshot: %w", err)
	}
	if err := replaceServiceContractEndpoints(ctx, tx, snapshotID, endpointRows); err != nil {
		return nil, err
	}
	if err := replaceServiceContractWebhooks(ctx, tx, snapshotID, webhookRows); err != nil {
		return nil, err
	}

	snapshot.ID = snapshotID
	snapshot.FetchedAt = fetchedAt
	snapshot.RefreshedAt = refreshedAt
	return &snapshot, nil
}

func validateServiceContractSnapshot(snapshot ServiceContractSnapshot) error {
	if err := fusedobject.ValidateExecutionContractEnvelope(snapshot.ExecutionContractEnvelope); err != nil {
		return fmt.Errorf("service contract snapshot: %w", err)
	}
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

// prepareServiceContractEndpointRows serializes once before batching so query
// count scales with bounded batches rather than operation cardinality.
func prepareServiceContractEndpointRows(endpoints []fusedobject.Endpoint) ([]serviceContractEndpointRow, error) {
	rows := make([]serviceContractEndpointRow, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint.ID == uuid.Nil {
			return nil, fmt.Errorf("replace service contract endpoints: endpoint %q has no id", endpoint.Name)
		}
		payload, err := json.Marshal(endpoint)
		if err != nil {
			return nil, fmt.Errorf("replace service contract endpoints: marshal %s: %w", endpoint.Name, err)
		}
		rows = append(rows, serviceContractEndpointRow{
			EndpointID: endpoint.ID, Name: endpoint.Name, Method: endpoint.Method,
			Path: endpoint.Path, NormalizedPath: endpoint.NormalizedPath, OperationJSON: payload,
		})
	}
	return rows, nil
}

// prepareServiceContractWebhookRows mirrors endpoint preparation to keep the
// transactional batch formula independent of webhook cardinality.
func prepareServiceContractWebhookRows(webhooks []fusedobject.Webhook) ([]serviceContractWebhookRow, error) {
	rows := make([]serviceContractWebhookRow, 0, len(webhooks))
	for _, webhook := range webhooks {
		if webhook.ID == uuid.Nil {
			return nil, fmt.Errorf("replace service contract webhooks: webhook %q has no id", webhook.Name)
		}
		payload, err := json.Marshal(webhook)
		if err != nil {
			return nil, fmt.Errorf("replace service contract webhooks: marshal %s: %w", webhook.Name, err)
		}
		rows = append(rows, serviceContractWebhookRow{
			WebhookID: webhook.ID, Name: webhook.Name, Method: webhook.Method, WebhookJSON: payload,
		})
	}
	return rows, nil
}

func replaceServiceContractEndpoints(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, rows []serviceContractEndpointRow) error {
	if _, err := tx.Exec(ctx, `DELETE FROM fused_service_contract_endpoints WHERE snapshot_id = $1`, snapshotID); err != nil {
		return fmt.Errorf("replace service contract endpoints: delete: %w", err)
	}
	return writeServiceContractBatches(rows, func(payload []byte) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO fused_service_contract_endpoints (
				snapshot_id, endpoint_id, name, method, path, normalized_path, operation_json
			)
			SELECT $1, records.endpoint_id, records.name, records.method,
			       records.path, records.normalized_path, records.operation_json
			FROM jsonb_to_recordset($2::jsonb) AS records(
				endpoint_id uuid, name text, method text, path text,
				normalized_path text, operation_json jsonb
			)`, snapshotID, payload); err != nil {
			return fmt.Errorf("replace service contract endpoints: insert batch: %w", err)
		}
		return nil
	})
}

func replaceServiceContractWebhooks(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, rows []serviceContractWebhookRow) error {
	if _, err := tx.Exec(ctx, `DELETE FROM fused_service_contract_webhooks WHERE snapshot_id = $1`, snapshotID); err != nil {
		return fmt.Errorf("replace service contract webhooks: delete: %w", err)
	}
	return writeServiceContractBatches(rows, func(payload []byte) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO fused_service_contract_webhooks (
				snapshot_id, webhook_id, name, method, webhook_json
			)
			SELECT $1, records.webhook_id, records.name, records.method, records.webhook_json
			FROM jsonb_to_recordset($2::jsonb) AS records(
				webhook_id uuid, name text, method text, webhook_json jsonb
			)`, snapshotID, payload); err != nil {
			return fmt.Errorf("replace service contract webhooks: insert batch: %w", err)
		}
		return nil
	})
}

func writeServiceContractBatches[T any](rows []T, write func([]byte) error) error {
	for start := 0; start < len(rows); start += serviceContractSnapshotWriteBatchSize {
		// Row-bounded batches prevent unusually large contracts from turning a
		// single statement into an unbounded PostgreSQL protocol payload.
		payload, err := json.Marshal(rows[start:min(start+serviceContractSnapshotWriteBatchSize, len(rows))])
		if err != nil {
			return fmt.Errorf("marshal service contract write batch: %w", err)
		}
		if err := write(payload); err != nil {
			return err
		}
	}
	return nil
}

func (s *postgresStore) GetServiceContractMetadata(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*fusedobject.ServiceMetadata, error) {
	var envelope fusedobject.ExecutionContractEnvelope
	var payload []byte
	err := s.db.QueryRow(ctx, `
		SELECT contract_version, required_capabilities, service_metadata
		FROM fused_service_contract_snapshots
		WHERE service_id = $1 AND service_version_id = $2`,
		serviceID, serviceVersionID,
	).Scan(&envelope.ContractVersion, &envelope.RequiredCapabilities, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrServiceContractSnapshotNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := validatePersistedExecutionContract(envelope); err != nil {
		return nil, err
	}
	var metadata fusedobject.ServiceMetadata
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return nil, fmt.Errorf("decode service contract metadata: %w", err)
	}
	metadata.ExecutionContractEnvelope = envelope
	return &metadata, nil
}

func (s *postgresStore) GetServiceContractEndpointByName(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointName string) (*fusedobject.Endpoint, error) {
	var envelope fusedobject.ExecutionContractEnvelope
	var payload []byte
	err := s.db.QueryRow(ctx, `
		SELECT snapshots.contract_version, snapshots.required_capabilities, endpoints.operation_json
		FROM fused_service_contract_snapshots snapshots
		LEFT JOIN fused_service_contract_endpoints endpoints
		  ON endpoints.snapshot_id = snapshots.id
		 AND endpoints.name = $3
		WHERE snapshots.service_id = $1
		  AND snapshots.service_version_id = $2`,
		serviceID, serviceVersionID, endpointName,
	).Scan(&envelope.ContractVersion, &envelope.RequiredCapabilities, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrServiceContractSnapshotNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := validatePersistedExecutionContract(envelope); err != nil {
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

// ListServiceContractEndpointsForSelections returns exact service contract endpoints for selections through one app-scoped query or cache lookup.
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
			            THEN item->'endpoint_ids' ELSE '[]'::jsonb END AS endpoint_ids,
			       CASE WHEN jsonb_typeof(item->'operation_names') = 'array'
			            THEN item->'operation_names' ELSE '[]'::jsonb END AS operation_names,
			       CASE WHEN jsonb_typeof(item->'endpoint_names') = 'array'
			            THEN item->'endpoint_names' ELSE '[]'::jsonb END AS endpoint_names
			FROM jsonb_array_elements($1::jsonb) AS item
		), resolved AS (
			SELECT requested.*, snapshots.id AS snapshot_id
			FROM requested
			LEFT JOIN fused_service_contract_snapshots snapshots
			  ON snapshots.service_id = requested.service_id
			 AND snapshots.service_version_id = requested.service_version_id
		)
		SELECT resolved.selection_index, resolved.snapshot_id,
		       snapshots.contract_version, snapshots.required_capabilities,
		       endpoints.operation_json
		FROM resolved
		LEFT JOIN fused_service_contract_snapshots snapshots
		  ON snapshots.id = resolved.snapshot_id
		LEFT JOIN fused_service_contract_endpoints endpoints
		  ON endpoints.snapshot_id = resolved.snapshot_id
		 AND (COALESCE(cardinality($2::text[]), 0) = 0 OR endpoints.name = ANY($2::text[]))
		 AND (
		   jsonb_array_length(resolved.endpoint_names) = 0
		   OR EXISTS (
		     SELECT 1
		     FROM jsonb_array_elements_text(resolved.endpoint_names) allowed(endpoint_name)
		     WHERE allowed.endpoint_name = endpoints.name
		   )
		 )
		 AND (
		   resolved.select_all
		   OR EXISTS (
		     SELECT 1
		     FROM jsonb_array_elements_text(resolved.endpoint_ids) allowed(endpoint_id)
		     WHERE allowed.endpoint_id::uuid = endpoints.endpoint_id
		   )
		   OR EXISTS (
		     SELECT 1
		     FROM jsonb_array_elements_text(resolved.operation_names) allowed(operation_name)
		     WHERE allowed.operation_name = endpoints.name
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
		var contractVersion *int
		var requiredCapabilities []string
		var operationJSON []byte
		if err := rows.Scan(&selectionIndex, &snapshotID, &contractVersion, &requiredCapabilities, &operationJSON); err != nil {
			return nil, err
		}
		if snapshotID == nil || contractVersion == nil {
			return nil, ErrServiceContractSnapshotNotFound
		}
		envelope := fusedobject.ExecutionContractEnvelope{ContractVersion: *contractVersion, RequiredCapabilities: requiredCapabilities}
		if err := validatePersistedExecutionContract(envelope); err != nil {
			return nil, err
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
	var envelope fusedobject.ExecutionContractEnvelope
	err := s.db.QueryRow(ctx, `
		SELECT id, contract_version, required_capabilities
		FROM fused_service_contract_snapshots
		WHERE service_id = $1 AND service_version_id = $2`,
		serviceID, serviceVersionID,
	).Scan(&snapshotID, &envelope.ContractVersion, &envelope.RequiredCapabilities)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrServiceContractSnapshotNotFound
	}
	if err != nil {
		return uuid.Nil, err
	}
	if err := validatePersistedExecutionContract(envelope); err != nil {
		return uuid.Nil, err
	}
	return snapshotID, nil
}

func validatePersistedExecutionContract(envelope fusedobject.ExecutionContractEnvelope) error {
	if err := fusedobject.ValidateExecutionContractEnvelope(envelope); err != nil {
		return fmt.Errorf("persisted service contract: %w", err)
	}
	return nil
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
	stable := serviceContractHashInput{
		ContractVersion:      snapshot.ContractVersion,
		RequiredCapabilities: append([]string{}, snapshot.RequiredCapabilities...),
		ServiceMetadata:      snapshot.ServiceMetadata,
		Endpoints:            append([]fusedobject.Endpoint(nil), snapshot.Endpoints...),
		Webhooks:             append([]fusedobject.Webhook(nil), snapshot.Webhooks...),
	}
	sort.Strings(stable.RequiredCapabilities)
	sort.Slice(stable.Endpoints, func(i, j int) bool {
		return stable.Endpoints[i].ID.String() < stable.Endpoints[j].ID.String()
	})
	sort.Slice(stable.Webhooks, func(i, j int) bool {
		return stable.Webhooks[i].ID.String() < stable.Webhooks[j].ID.String()
	})
	stable, err := normalizeServiceContractHashInput(stable)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(stable)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
