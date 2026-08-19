package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// ExactOperationBinding is the immutable physical identity compiled into one
// Unified target. All four values must agree with one selection in the exact
// authenticated app version and one operation in the local contract snapshot.
type ExactOperationBinding struct {
	ServiceID        uuid.UUID
	ServiceVersionID uuid.UUID
	EndpointID       uuid.UUID
	EndpointName     string
}

// ResolvedPhysicalOperation is intentionally opaque outside this package so a
// caller cannot replace snapshot-authorized routing data after prevalidation.
type ResolvedPhysicalOperation struct {
	appID uuid.UUID
	match *scopedEndpoint
}

type exactBindingEndpointLister interface {
	ListExactBindingEndpoints(context.Context, []models.SDKSelection, []ExactOperationBinding) (map[int]fusedobject.Endpoint, error)
}

// ResolveExactPhysicalOperations validates every binding as one batch before
// any provider call starts. Returned operations preserve binding order.
func ResolveExactPhysicalOperations(ctx context.Context, cache ObjectCache, appID uuid.UUID, bindings []ExactOperationBinding) ([]ResolvedPhysicalOperation, error) {
	if appID == uuid.Nil || len(bindings) == 0 {
		return nil, errors.New("exact physical operation scope is empty")
	}
	selections, err := validateAndParseScope(ctx, cache, appID.String())
	if err != nil {
		return nil, err
	}
	aligned, err := alignExactBindingsToScope(selections, bindings)
	if err != nil {
		return nil, err
	}
	endpoints, err := listExactBindingEndpoints(ctx, cache, aligned, bindings)
	if err != nil {
		return nil, err
	}
	return buildResolvedPhysicalOperations(ctx, cache, appID, aligned, bindings, endpoints)
}

// alignExactBindingsToScope pairs each exact binding with its compiled app selection without matching by display name.
func alignExactBindingsToScope(selections []models.SDKSelection, bindings []ExactOperationBinding) ([]models.SDKSelection, error) {
	aligned := make([]models.SDKSelection, len(bindings))
	for index, binding := range bindings {
		if err := validateExactOperationBinding(binding); err != nil {
			return nil, err
		}
		selection, found := findExactBindingSelection(selections, binding)
		if !found {
			return nil, errors.New("ScopeError: exact operation binding is outside app scope")
		}
		aligned[index] = selection
	}
	return aligned, nil
}

// validateExactOperationBinding rejects malformed exact operation binding before it can cross the exact physical operation resolution boundary.
func validateExactOperationBinding(binding ExactOperationBinding) error {
	if binding.ServiceID == uuid.Nil || binding.ServiceVersionID == uuid.Nil || binding.EndpointID == uuid.Nil {
		return errors.New("exact operation binding contains an empty identity")
	}
	if strings.TrimSpace(binding.EndpointName) == "" || binding.EndpointName != strings.TrimSpace(binding.EndpointName) {
		return errors.New("exact operation binding contains an invalid endpoint name")
	}
	return nil
}

// findExactBindingSelection locates the one compiled selection whose service and version own an exact binding.
func findExactBindingSelection(selections []models.SDKSelection, binding ExactOperationBinding) (models.SDKSelection, bool) {
	for _, selection := range selections {
		if selection.ServiceID == binding.ServiceID && selection.ServiceVersionID == binding.ServiceVersionID {
			return selection, true
		}
	}
	return models.SDKSelection{}, false
}

// listExactBindingEndpoints performs one set-based endpoint snapshot query for all admitted bindings.
func listExactBindingEndpoints(ctx context.Context, cache ObjectCache, selections []models.SDKSelection, bindings []ExactOperationBinding) (map[int]fusedobject.Endpoint, error) {
	lister, ok := cache.(exactBindingEndpointLister)
	if !ok {
		return nil, errors.New("exact endpoint snapshot resolution is unavailable")
	}
	return lister.ListExactBindingEndpoints(ctx, selections, bindings)
}

// buildResolvedPhysicalOperations combines aligned metadata and endpoints into opaque app-scoped operations in request order.
func buildResolvedPhysicalOperations(
	ctx context.Context,
	cache ObjectCache,
	appID uuid.UUID,
	selections []models.SDKSelection,
	bindings []ExactOperationBinding,
	endpoints map[int]fusedobject.Endpoint,
) ([]ResolvedPhysicalOperation, error) {
	resolved := make([]ResolvedPhysicalOperation, len(bindings))
	services := make(map[string]*fusedobject.ServiceMetadata)
	for index, binding := range bindings {
		operation, err := resolvedPhysicalOperationForBinding(ctx, cache, appID, selections[index], binding, endpoints[index], services)
		if err != nil {
			return nil, err
		}
		resolved[index] = operation
	}
	return resolved, nil
}

// resolvedPhysicalOperationForBinding rejects any endpoint identity mismatch before constructing the opaque physical handle.
func resolvedPhysicalOperationForBinding(
	ctx context.Context,
	cache ObjectCache,
	appID uuid.UUID,
	selection models.SDKSelection,
	binding ExactOperationBinding,
	endpoint fusedobject.Endpoint,
	services map[string]*fusedobject.ServiceMetadata,
) (ResolvedPhysicalOperation, error) {
	if endpoint.ID != binding.EndpointID || endpoint.Name != binding.EndpointName || !endpointAllowed(selection, &endpoint) {
		return ResolvedPhysicalOperation{}, errors.New("ScopeError: exact operation binding does not match app scope snapshot")
	}
	service, err := exactBindingServiceMetadata(ctx, cache, appID, binding, services)
	if err != nil {
		return ResolvedPhysicalOperation{}, err
	}
	match := &scopedEndpoint{
		service: service, endpoint: endpoint, allowed: true,
		serviceVersionID: binding.ServiceVersionID.String(), selection: selection,
	}
	return ResolvedPhysicalOperation{appID: appID, match: match}, nil
}

// exactBindingServiceMetadata reuses metadata per service version while assembling resolved operations.
func exactBindingServiceMetadata(
	ctx context.Context,
	cache ObjectCache,
	appID uuid.UUID,
	binding ExactOperationBinding,
	services map[string]*fusedobject.ServiceMetadata,
) (*fusedobject.ServiceMetadata, error) {
	key := binding.ServiceID.String() + ":" + binding.ServiceVersionID.String()
	if service := services[key]; service != nil {
		return service, nil
	}
	service, err := cache.GetOrFetchServiceMetadata(ctx, appID.String(), binding.ServiceID.String())
	if err != nil {
		return nil, err
	}
	if service == nil || service.ID != binding.ServiceID || service.ServiceVersionID != binding.ServiceVersionID {
		return nil, errors.New("ScopeError: exact service binding does not match app scope snapshot")
	}
	if err := fusedobject.ValidateExecutionContractEnvelope(service.ExecutionContractEnvelope); err != nil {
		return nil, err
	}
	services[key] = service
	return service, nil
}

// ListExactBindingEndpoints performs one set-based snapshot lookup for every
// prevalidated binding. SelectionIndex is the binding's stable request order.
func (cache *LocalObjectCache) ListExactBindingEndpoints(
	ctx context.Context,
	selections []models.SDKSelection,
	bindings []ExactOperationBinding,
) (map[int]fusedobject.Endpoint, error) {
	contractStore := cache.serviceContractStore()
	if contractStore == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	requested := exactEndpointSelections(selections, bindings)
	matches, err := contractStore.ListServiceContractEndpointsForSelections(ctx, requested, nil)
	if err != nil {
		return nil, err
	}
	return indexExactEndpointMatches(matches)
}

// exactEndpointSelections converts exact bindings into one set-based snapshot query payload.
func exactEndpointSelections(selections []models.SDKSelection, bindings []ExactOperationBinding) []store.ServiceContractEndpointSelection {
	requested := make([]store.ServiceContractEndpointSelection, len(bindings))
	for index, binding := range bindings {
		selection := selections[index]
		requested[index] = store.ServiceContractEndpointSelection{
			SelectionIndex: index, ServiceID: binding.ServiceID, ServiceVersionID: binding.ServiceVersionID,
			SelectAll: selection.SelectAll, EndpointIDs: selection.EndpointIDs,
			OperationNames: selection.OperationNames, EndpointNames: []string{binding.EndpointName},
		}
	}
	return requested
}

// indexExactEndpointMatches keys snapshot results by position so duplicate operation names cannot alias.
func indexExactEndpointMatches(matches []store.ServiceContractEndpointMatch) (map[int]fusedobject.Endpoint, error) {
	indexed := make(map[int]fusedobject.Endpoint, len(matches))
	for _, match := range matches {
		if _, duplicate := indexed[match.SelectionIndex]; duplicate {
			return nil, fmt.Errorf("exact endpoint snapshot returned duplicate selection %d", match.SelectionIndex)
		}
		indexed[match.SelectionIndex] = match.Endpoint
	}
	return indexed, nil
}
