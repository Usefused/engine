package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// InlineSDKContractStore exports admitted local snapshots for Cloud-pinned services
// when an Enterprise Local Registry compiles a mixed Local+Cloud SDK.
type InlineSDKContractStore interface {
	BuildInlineSDKServiceContracts(ctx context.Context, refs []ServiceContractMetadataRef) ([]models.InlineSDKServiceContract, error)
}

// BuildInlineSDKServiceContracts loads complete credential-free service contracts from local snapshots.
func (s *postgresStore) BuildInlineSDKServiceContracts(ctx context.Context, refs []ServiceContractMetadataRef) ([]models.InlineSDKServiceContract, error) {
	// Empty Cloud subsets require no snapshot queries.
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]models.InlineSDKServiceContract, 0, len(refs))
	for _, ref := range refs {
		contract, err := s.buildSingleInlineSDKServiceContract(ctx, ref)
		// Any missing or corrupt local snapshot aborts mixed SDK compilation fail-closed.
		if err != nil {
			return nil, err
		}
		out = append(out, contract)
	}
	return out, nil
}

// buildSingleInlineSDKServiceContract projects one persisted ServiceContractSnapshot into the wire contract.
func (s *postgresStore) buildSingleInlineSDKServiceContract(ctx context.Context, ref ServiceContractMetadataRef) (models.InlineSDKServiceContract, error) {
	snapshotID, versionName, metadata, err := s.loadInlineSnapshotHeader(ctx, ref)
	// Snapshot header validation must succeed before reading child operations or webhooks.
	if err != nil {
		return models.InlineSDKServiceContract{}, err
	}
	slug := s.lookupWorkspaceServiceSlug(ctx, ref.ServiceID)
	endpoints, err := s.loadInlineSnapshotEndpoints(ctx, snapshotID, ref.ServiceID)
	// Corrupt or unreadable operation rows must never produce a partial SDK.
	if err != nil {
		return models.InlineSDKServiceContract{}, err
	}
	webhooks, err := s.loadInlineSnapshotWebhooks(ctx, snapshotID, ref.ServiceID)
	// Corrupt or unreadable webhook rows must never produce a partial SDK.
	if err != nil {
		return models.InlineSDKServiceContract{}, err
	}
	return assembleInlineSDKServiceContract(ref, versionName, slug, metadata, endpoints, webhooks)
}

// loadInlineSnapshotHeader reads and validates the snapshot envelope, version label, and metadata JSON.
func (s *postgresStore) loadInlineSnapshotHeader(ctx context.Context, ref ServiceContractMetadataRef) (uuid.UUID, string, *fusedobject.ServiceMetadata, error) {
	var snapshotID uuid.UUID
	var versionName string
	var envelope fusedobject.ExecutionContractEnvelope
	var payload []byte
	err := s.db.QueryRow(ctx, `
		SELECT id, version, contract_version, required_capabilities, service_metadata
		FROM fused_service_contract_snapshots
		WHERE service_id = $1 AND service_version_id = $2`,
		ref.ServiceID, ref.ServiceVersionID,
	).Scan(&snapshotID, &versionName, &envelope.ContractVersion, &envelope.RequiredCapabilities, &payload)
	// Reject missing snapshots or storage failures before unmarshaling metadata.
	if err != nil {
		return uuid.Nil, "", nil, err
	}
	// Revalidate execution capability support against this Engine build.
	if err := validatePersistedExecutionContract(envelope); err != nil {
		return uuid.Nil, "", nil, err
	}
	var metadata fusedobject.ServiceMetadata
	// Reject malformed metadata JSON before building generator inputs.
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return uuid.Nil, "", nil, fmt.Errorf("decode inline contract metadata: %w", err)
	}
	metadata.ExecutionContractEnvelope = envelope
	return snapshotID, versionName, &metadata, nil
}

// lookupWorkspaceServiceSlug extracts the bare slug portion from fused_workspace_services when present.
func (s *postgresStore) lookupWorkspaceServiceSlug(ctx context.Context, serviceID uuid.UUID) string {
	var reference string
	// Slug lookup is best-effort naming metadata; fall back to empty string if absent.
	if err := s.db.QueryRow(ctx, `SELECT service_slug FROM fused_workspace_services WHERE service_id = $1`, serviceID).Scan(&reference); err != nil {
		return ""
	}
	// Strip canonical @provider/ prefixes so the generator receives the bare service slug.
	if strings.HasPrefix(reference, "@") {
		if _, bare, ok := strings.Cut(reference[1:], "/"); ok {
			return bare
		}
	}
	return reference
}

// loadInlineSnapshotEndpoints decodes all persisted operations for snapshotID into models.IntegrationObject values.
func (s *postgresStore) loadInlineSnapshotEndpoints(ctx context.Context, snapshotID, serviceID uuid.UUID) ([]models.IntegrationObject, error) {
	endpoints, err := s.listServiceContractEndpoints(ctx, `
		SELECT operation_json
		FROM fused_service_contract_endpoints
		WHERE snapshot_id = $1
		ORDER BY name`, snapshotID)
	// Propagate query or endpoint-JSON decode failures.
	if err != nil {
		return nil, err
	}
	out := make([]models.IntegrationObject, 0, len(endpoints))
	for _, ep := range endpoints {
		converted, convErr := endpointToModelIntegrationObject(ep, serviceID)
		// Abort if any operation fails round-trip conversion into the shared model shape.
		if convErr != nil {
			return nil, convErr
		}
		out = append(out, converted)
	}
	return out, nil
}

// endpointToModelIntegrationObject converts a fusedobject.Endpoint into models.IntegrationObject preserving resource naming.
func endpointToModelIntegrationObject(ep fusedobject.Endpoint, serviceID uuid.UUID) (models.IntegrationObject, error) {
	raw, err := json.Marshal(ep)
	// Serialization of an already-validated endpoint must succeed.
	if err != nil {
		return models.IntegrationObject{}, err
	}
	var obj models.IntegrationObject
	// Shared JSON tags translate parameter, request, response, and security contracts cleanly.
	if err := json.Unmarshal(raw, &obj); err != nil {
		return models.IntegrationObject{}, err
	}
	obj.ServiceID = serviceID
	obj.ResourceName = ep.ResourceName
	return obj, nil
}

// loadInlineSnapshotWebhooks decodes all persisted webhooks for snapshotID into models.WebhookObject values.
func (s *postgresStore) loadInlineSnapshotWebhooks(ctx context.Context, snapshotID, serviceID uuid.UUID) ([]models.WebhookObject, error) {
	rows, err := s.db.Query(ctx, `
		SELECT webhook_json
		FROM fused_service_contract_webhooks
		WHERE snapshot_id = $1
		ORDER BY name`, snapshotID)
	// Surface database query errors before iterating rows.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.WebhookObject
	for rows.Next() {
		var payload []byte
		// Abort on row scan failure.
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var wh models.WebhookObject
		// Reject malformed webhook JSON payloads.
		if err := json.Unmarshal(payload, &wh); err != nil {
			return nil, fmt.Errorf("decode inline contract webhook: %w", err)
		}
		wh.ServiceID = serviceID
		out = append(out, wh)
	}
	return out, rows.Err()
}

// assembleInlineSDKServiceContract translates ServiceMetadata and child slices into models.InlineSDKServiceContract.
func assembleInlineSDKServiceContract(
	ref ServiceContractMetadataRef,
	versionName, slug string,
	metadata *fusedobject.ServiceMetadata,
	endpoints []models.IntegrationObject,
	webhooks []models.WebhookObject,
) (models.InlineSDKServiceContract, error) {
	rawMeta, err := json.Marshal(metadata)
	// Metadata marshaling bridges fusedobject policy aliases into models types without lossy manual copying.
	if err != nil {
		return models.InlineSDKServiceContract{}, err
	}
	var contract models.InlineSDKServiceContract
	// Shared JSON field names populate servers, auth_configs, rate_limit, retry_config, pagination, and schema_definitions.
	if err := json.Unmarshal(rawMeta, &contract); err != nil {
		return models.InlineSDKServiceContract{}, err
	}
	contract.ServiceID = ref.ServiceID
	contract.ServiceVersionID = ref.ServiceVersionID
	contract.Version = versionName
	contract.ContractVersion = metadata.ContractVersion
	contract.RequiredCapabilities = append([]string(nil), metadata.RequiredCapabilities...)
	contract.Slug = slug
	contract.Endpoints = endpoints
	contract.Webhooks = webhooks
	digest, err := contract.ComputeDigest()
	// Bind the exact exported contract bytes so Local Registry verifies integrity and caches by content digest.
	if err != nil {
		return models.InlineSDKServiceContract{}, fmt.Errorf("hash inline SDK contract: %w", err)
	}
	contract.ContentSHA256 = digest
	return contract, nil
}
