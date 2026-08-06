package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// buildSessionFixture derives one MCP session's operation catalog from the
// connecting SDK's own AppRuntime.Selections. Every shared-runtime session gets
// its own scoped
// fixture: only the endpoints its owner actually selected are discoverable
// via search_docs or callable via call(), matching the per-SDK enforcement
// engineExecuteCore/findEndpointInScope already apply at dispatch time --
// this makes the *catalog* match the *enforcement*, not just the enforcement
// alone.
func buildSessionFixture(ctx context.Context, cache ObjectCache, appID string, selections []models.SDKSelection) (*Fixture, error) {
	var ops []FixtureOperation
	for _, sel := range selections {
		endpoints, err := cache.ListEndpointsForSelection(ctx, appID, sel)
		if err != nil {
			return nil, fmt.Errorf("list endpoints for service %s: %w", sel.ServiceID, err)
		}
		for _, ep := range endpoints {
			op, err := endpointToFixtureOperation(sel.ServiceID.String(), ep)
			if err != nil {
				return nil, fmt.Errorf("convert endpoint %s: %w", ep.Name, err)
			}
			stripMCPAuthParameters(&op, sel.AuthName)
			ops = append(ops, op)
		}
	}
	return newFixtureFromOperations(ctx, ops), nil
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

// endpointToFixtureOperation maps a Registry-resolved endpoint onto the
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
func endpointToFixtureOperation(serviceID string, ep fusedobject.Endpoint) (FixtureOperation, error) {
	raw, err := json.Marshal(ep)
	if err != nil {
		return FixtureOperation{}, fmt.Errorf("marshal endpoint: %w", err)
	}
	var op FixtureOperation
	if err := json.Unmarshal(raw, &op); err != nil {
		return FixtureOperation{}, fmt.Errorf("unmarshal endpoint into fixture operation: %w", err)
	}
	op.OperationID = ep.Name
	op.ServiceID = serviceID
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
// authoring mistake rather than a live scope's cross-vendor name collision.
func newFixtureFromOperations(ctx context.Context, ops []FixtureOperation) *Fixture {
	f := &Fixture{}
	seen := make(map[string]struct{}, len(ops))
	for _, op := range ops {
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
