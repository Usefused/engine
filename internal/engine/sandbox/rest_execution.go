package sandbox

import (
	"context"
	"errors"
	"strings"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// ErrPhysicalOperationAmbiguous identifies an operation name selected from
// more than one immutable physical service contract.
var ErrPhysicalOperationAmbiguous = errors.New("physical operation name is ambiguous")

// ConnectAppRuntime acquires one request-scoped reference to the immutable app
// cache used by both physical and Unified in-process execution.
func (*EngineGRPCServer) ConnectAppRuntime(ctx context.Context, appID uuid.UUID) error {
	if globalObjectCache == nil {
		return errors.New("Engine runtime cache is unavailable")
	}
	return globalObjectCache.ConnectSDK(ctx, appID.String())
}

// DisconnectAppRuntime releases the exact cache reference acquired for one
// REST request; disconnect remains best-effort during response teardown.
func (*EngineGRPCServer) DisconnectAppRuntime(appID uuid.UUID) {
	if globalObjectCache != nil && appID != uuid.Nil {
		globalObjectCache.DisconnectSDK(appID.String())
	}
}

// ResolvePhysicalOperationByName resolves one exact public operation against
// every immutable selection, including select-all snapshot contents.
func (*EngineGRPCServer) ResolvePhysicalOperationByName(ctx context.Context, appID uuid.UUID, operation string) (ResolvedPhysicalOperation, bool, error) {
	if globalObjectCache == nil {
		return ResolvedPhysicalOperation{}, false, errors.New("Engine runtime cache is unavailable")
	}
	return resolvePhysicalOperationByName(ctx, globalObjectCache, appID, operation)
}

// resolvePhysicalOperationByName uses the batch snapshot lookup so operation
// existence never depends on naming syntax or incomplete OperationNames lists.
func resolvePhysicalOperationByName(ctx context.Context, cache ObjectCache, appID uuid.UUID, operation string) (ResolvedPhysicalOperation, bool, error) {
	if appID == uuid.Nil || operation == "" || operation != strings.TrimSpace(operation) {
		return ResolvedPhysicalOperation{}, false, errors.New("physical operation lookup is invalid")
	}
	selections, err := validateAndParseScope(ctx, cache, appID.String())
	if err != nil {
		return ResolvedPhysicalOperation{}, false, err
	}
	lister, ok := cache.(endpointBatchLister)
	if !ok {
		return ResolvedPhysicalOperation{}, false, errors.New("app-scoped endpoint lookup unavailable")
	}
	grouped, err := lister.ListEndpointsForSelections(ctx, selections, []string{operation})
	if err != nil {
		return ResolvedPhysicalOperation{}, false, err
	}
	return resolvedPhysicalCandidate(ctx, cache, appID, operation, selections, grouped)
}

// resolvedPhysicalCandidate rejects cross-service duplicates instead of
// silently using selection order as routing authority for the REST surface.
func resolvedPhysicalCandidate(ctx context.Context, cache ObjectCache, appID uuid.UUID, operation string, selections []models.SDKSelection, grouped map[int][]fusedobject.Endpoint) (ResolvedPhysicalOperation, bool, error) {
	var candidate ResolvedPhysicalOperation
	found := false
	services := make(map[string]*fusedobject.ServiceMetadata)
	for index, selection := range selections {
		for _, endpoint := range grouped[index] {
			if endpoint.Name != operation || !endpointAllowed(selection, &endpoint) {
				continue
			}
			if found {
				return ResolvedPhysicalOperation{}, false, ErrPhysicalOperationAmbiguous
			}
			binding := ExactOperationBinding{
				ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID,
				EndpointID: endpoint.ID, EndpointName: endpoint.Name,
			}
			resolved, err := resolvedPhysicalOperationForBinding(ctx, cache, appID, selection, binding, endpoint, services)
			if err != nil {
				return ResolvedPhysicalOperation{}, false, err
			}
			candidate, found = resolved, true
		}
	}
	return candidate, found, nil
}
