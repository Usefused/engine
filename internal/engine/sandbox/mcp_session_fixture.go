package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// buildSessionFixture derives one MCP session's operation catalog from the
// connecting app version's AppRuntime.Selections. Every shared-runtime session
// gets its own scoped fixture: only the endpoints its owner actually selected
// are discoverable via search_docs or callable via call(), matching the per-app
// enforcement engineExecuteCore/findEndpointInScope apply at dispatch time --
// this makes the *catalog* match the *enforcement*, not just the enforcement
// alone.
func buildSessionFixture(ctx context.Context, cache ObjectCache, appID string, selections []models.SDKSelection, policy store.AppTokenPolicy) (*Fixture, error) {
	names := policy.AllowedOperations
	// Unrestricted tokens preserve the existing nil predicate that asks SQL for
	// every physical operation selected by the immutable app version.
	if policy.IsUnrestricted() {
		names = nil
	}
	fixture, err := buildBatchedSessionFixture(ctx, cache, selections, names)
	// Physical catalogue failure remains authoritative and must not be hidden by
	// an independently available logical descriptor.
	if err != nil {
		return nil, err
	}
	loader, ok := cache.(mcpUnifiedDescriptorLoader)
	// A cache without the Engine-local applied-plan resolver cannot claim to
	// provide a complete catalogue or reconstruct one from private definitions.
	if !ok {
		return nil, fmt.Errorf("MCP Unified descriptor lookup unavailable")
	}
	descriptors, err := loader.GetMCPUnifiedOperationDescriptors(ctx, appID, policy)
	// Missing or inconsistent applied-plan state makes the complete catalogue
	// unavailable rather than silently degrading to physical-only discovery.
	if err != nil {
		return nil, err
	}
	if err := fixture.attachUnifiedOperations(descriptors); err != nil {
		return nil, err
	}
	metadataLoader, ok := cache.(mcpServerMetadataLoader)
	// Server identity must describe the same immutable app version as the catalogue, never a process-wide runtime.
	if !ok {
		return nil, fmt.Errorf("MCP server metadata lookup unavailable")
	}
	metadata, err := metadataLoader.GetMCPServerMetadata(ctx, appID)
	// Persistence failures must not silently relabel one app as a generic shared server.
	if err != nil {
		return nil, err
	}
	fixture.Server, err = validateMCPServerMetadata(metadata)
	// Missing authored identity is an invalid app version, not a reason to invent generic server capabilities.
	if err != nil {
		return nil, err
	}
	return fixture, nil
}

type mcpUnifiedDescriptorLoader interface {
	GetMCPUnifiedOperationDescriptors(context.Context, string, store.AppTokenPolicy) (*models.SDKUnifiedOperationDescriptors, error)
}

type mcpServerMetadataLoader interface {
	GetMCPServerMetadata(context.Context, string) (FixtureServerMetadata, error)
}

// GetMCPServerMetadata reads the exact immutable runtime identity without retaining another metadata cache.
func (c *LocalObjectCache) GetMCPServerMetadata(ctx context.Context, appID string) (FixtureServerMetadata, error) {
	parsedAppID, err := uuid.Parse(appID)
	// A malformed transport identifier cannot be allowed to select a different app record.
	if err != nil {
		return FixtureServerMetadata{}, fmt.Errorf("invalid MCP app id: %w", err)
	}
	runtime, err := c.db.GetAppRuntime(ctx, parsedAppID)
	// The session must advertise only metadata belonging to its successfully loaded app version.
	if err != nil {
		return FixtureServerMetadata{}, fmt.Errorf("load MCP server metadata: %w", err)
	}
	return FixtureServerMetadata{
		Name: runtime.Name, Title: runtime.Name, Version: runtime.Version, Description: runtime.Description,
	}, nil
}

// validateMCPServerMetadata requires complete authored identity for every runnable MCP app version.
func validateMCPServerMetadata(metadata FixtureServerMetadata) (FixtureServerMetadata, error) {
	metadata.Name = strings.TrimSpace(metadata.Name)
	metadata.Title = strings.TrimSpace(metadata.Title)
	metadata.Version = strings.TrimSpace(metadata.Version)
	metadata.Description = strings.TrimSpace(metadata.Description)
	// Every field is part of the advertised immutable version; partial metadata must stop session creation.
	if metadata.Name == "" || metadata.Title == "" || metadata.Version == "" || metadata.Description == "" {
		return FixtureServerMetadata{}, fmt.Errorf("MCP app version is missing required server metadata")
	}
	// Defensive runtime admission mirrors config planning even when a fixture comes from another adapter.
	if len(metadata.Description) > models.MCPServerDescriptionMaxBytes {
		return FixtureServerMetadata{}, fmt.Errorf("MCP server description exceeds %d bytes", models.MCPServerDescriptionMaxBytes)
	}
	return metadata, nil
}

// GetMCPUnifiedOperationDescriptors delegates one session-scoped read to the
// persistence owner without retaining descriptor state in another cache.
func (c *LocalObjectCache) GetMCPUnifiedOperationDescriptors(ctx context.Context, appID string, policy store.AppTokenPolicy) (*models.SDKUnifiedOperationDescriptors, error) {
	parsedAppID, err := uuid.Parse(appID)
	// Exact UUID parsing prevents a malformed transport identifier from reaching
	// the applied-plan query under a different interpretation.
	if err != nil {
		return nil, fmt.Errorf("invalid MCP app id: %w", err)
	}
	repository, ok := c.db.(store.MCPUnifiedDescriptorStore)
	// Registry or private-definition fallbacks would create competing sources
	// of public catalogue truth, so absence fails closed.
	if !ok {
		return nil, fmt.Errorf("MCP Unified descriptor store unavailable")
	}
	return repository.GetMCPUnifiedOperationDescriptors(ctx, parsedAppID, policy.IsUnrestricted(), policy.AllowedOperations)
}

type endpointBatchLister interface {
	ListEndpointsForSelections(context.Context, []models.SDKSelection, []string) (map[int][]fusedobject.Endpoint, error)
}

// buildBatchedSessionFixture reuses the connected app metadata cache beside one set-based endpoint query.
func buildBatchedSessionFixture(ctx context.Context, cache ObjectCache, selections []models.SDKSelection, allowedOperations []string) (*Fixture, error) {
	lister, ok := cache.(endpointBatchLister)
	// Per-service fallback queries would reintroduce N+1 catalogue loading.
	if !ok {
		return nil, fmt.Errorf("app-scoped endpoint lookup unavailable")
	}
	endpointsBySelection, err := lister.ListEndpointsForSelections(ctx, selections, allowedOperations)
	// A failed batch cannot safely produce a partially authorized catalogue.
	if err != nil {
		return nil, fmt.Errorf("list app-scoped endpoints: %w", err)
	}
	servicePagination, err := mcpPaginationForSelections(ctx, cache, selections)
	// Effective policy documentation must come from the same immutable metadata already loaded for execution.
	if err != nil {
		return nil, err
	}
	var operations []FixtureOperation
	for index, selection := range selections {
		operations, err = appendFixtureOperations(operations, selection, endpointsBySelection[index], servicePagination[index])
		// Rejection of one selected operation prevents the entire unsafe fixture from reaching Node.
		if err != nil {
			return nil, err
		}
	}
	fixture := newFixtureFromOperations(ctx, operations)
	// Dictionary lookup is an in-memory batch over metadata preloaded during app connection.
	if err := attachMCPDefinitions(fixture, cache, selections); err != nil {
		return nil, err
	}
	return fixture, nil
}

// appendFixtureOperations pins each documentation operation to the same version as its shared schema dictionary.
func appendFixtureOperations(operations []FixtureOperation, selection models.SDKSelection, endpoints []fusedobject.Endpoint, servicePagination *fusedobject.PaginationConfig) ([]FixtureOperation, error) {
	for _, endpoint := range endpoints {
		operation, err := endpointToFixtureOperation(selection.ServiceID.String(), endpoint, servicePagination)
		// Conversion failures retain exact operation context without dropping a selected callable silently.
		if err != nil {
			return nil, fmt.Errorf("convert endpoint %s: %w", endpoint.Name, err)
		}
		stripMCPAuthParameters(&operation, selection.AuthName)
		operation.ServiceVersionID = selection.ServiceVersionID.String()
		operations = append(operations, operation)
	}
	return operations, nil
}

// stripMCPAuthParameters removes credential-shaped inputs before the operation
// reaches search_docs. Auth is injected after scope resolution, so advertising
// these fields only invites an agent to hallucinate or override credentials.
func stripMCPAuthParameters(operation *FixtureOperation, authName string) {
	filtered := operation.Parameters[:0]
	for _, parameter := range operation.Parameters {
		name := strings.TrimSpace(parameter.Name)
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, authName) || strings.HasPrefix(strings.ToLower(name), "fused_") {
			continue
		}
		filtered = append(filtered, parameter)
	}
	operation.Parameters = filtered
}

// endpointToFixtureOperation maps a locally snapshotted endpoint onto the
// fixture shape search_docs/call() expect. fusedobject.Endpoint and
// FixtureOperation/models.* are structurally identical on the wire (same
// json tags) but distinct Go types in different packages -- round-tripping
// through JSON reuses that shared shape without a hand-duplicated
// field-by-field converter that would silently drift if either side gains a
// field later (same reasoning mcp_fixture.go's own doc comment gives for
// reusing models.Parameter/models.Schema in the first place).
//
// OperationID is set explicitly to the endpoint's own Name, not left to the
// JSON round-trip (fusedobject.Endpoint has no operation_id field): this
// must match exactly what dispatch resolves against
// (mcp_shared_runtime.go's dispatchMCPCall passes call()'s operation_id
// straight through as engineExecuteCore's endpointName), so a session's
// fixture and its live dispatch path always mean the same thing by the same
// identifier.
func endpointToFixtureOperation(serviceID string, ep fusedobject.Endpoint, servicePagination *fusedobject.PaginationConfig) (FixtureOperation, error) {
	// Source schemas cross a JSON conversion before fixture admission, so the
	// same bounded preflight must run before that conversion can allocate or recurse.
	if err := validateMCPSourceEndpointSchemas(ep); err != nil {
		return FixtureOperation{}, err
	}
	raw, err := json.Marshal(ep)
	// Conversion errors are distinct from policy rejection and retain their existing classification.
	if err != nil {
		return FixtureOperation{}, fmt.Errorf("marshal endpoint: %w", err)
	}
	var op FixtureOperation
	// The shared wire shape must decode completely before the operation becomes discoverable.
	if err := json.Unmarshal(raw, &op); err != nil {
		return FixtureOperation{}, fmt.Errorf("unmarshal endpoint into fixture operation: %w", err)
	}
	op.OperationID = ep.Name
	op.ServiceID = serviceID
	op.Pagination = fixturePagination(ep.Pagination, servicePagination)
	return op, nil
}

// newFixtureFromOperations indexes operations by OperationID for a
// dynamically-built session fixture, tolerating (and logging) a duplicate
// name across two selected services rather than failing the whole session.
// findEndpointInScope's own dispatch resolution already has this same
// "first match across selections wins" semantics for duplicate endpoint
// names (sandbox.go), so the fixture describing that dispatch path
// shouldn't be stricter than the path itself. LoadFixture keeps its own
// hard-fail-on-duplicate behavior for fixture files, where a duplicate is an
// authoring mistake rather than a live scope's cross-service name collision.
func newFixtureFromOperations(ctx context.Context, ops []FixtureOperation) *Fixture {
	// A fully narrowed token may expose no physical operations; Node still requires the catalogue field to be an array.
	f := &Fixture{Operations: make([]FixtureOperation, 0, len(ops))}
	seen := make(map[string]struct{}, len(ops))
	for _, op := range ops {
		// Live cross-service collisions preserve the first callable to match dispatch's existing resolution order.
		if _, exists := seen[op.OperationID]; exists {
			slog.WarnContext(ctx, "duplicate operation_id building session fixture; keeping first",
				slog.String("operation_id", op.OperationID))
			continue
		}
		seen[op.OperationID] = struct{}{}
		f.Operations = append(f.Operations, op)
	}

	f.byOperationID = make(map[string]*FixtureOperation, len(f.Operations))
	for i := range f.Operations {
		f.byOperationID[f.Operations[i].OperationID] = &f.Operations[i]
	}
	return f
}
