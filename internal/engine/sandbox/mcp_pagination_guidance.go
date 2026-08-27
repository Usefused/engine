package sandbox

import (
	"context"
	"errors"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
)

type mcpPaginationLoader interface {
	MCPPaginationForSelections(context.Context, []models.SDKSelection) (map[int]*fusedobject.PaginationConfig, error)
}

// MCPPaginationForSelections projects cached service fallbacks without adding catalogue-time queries.
func (cache *LocalObjectCache) MCPPaginationForSelections(ctx context.Context, selections []models.SDKSelection) (map[int]*fusedobject.PaginationConfig, error) {
	cache.mu.RLock()
	metadata := make([]*fusedobject.ServiceMetadata, len(selections))
	refs := make([]store.ServiceContractMetadataRef, len(selections))
	// One cache pass captures immutable metadata and constructs the batch override request together.
	for index, selection := range selections {
		version, err := selectionVersionIdentity(selection)
		// Policy lookup must use the same exact-version cache key as execution.
		if err != nil {
			cache.mu.RUnlock()
			return nil, err
		}
		metadata[index] = cache.serviceMetadataCache[selection.ServiceID.String()+":"+version]
		// Missing cached metadata would make service fallback guidance differ from execution.
		if metadata[index] == nil {
			cache.mu.RUnlock()
			return nil, errors.New("MCP service pagination metadata unavailable")
		}
		refs[index] = store.ServiceContractMetadataRef{ServiceID: selection.ServiceID, ServiceVersionID: selection.ServiceVersionID}
	}
	cache.mu.RUnlock()
	overrides := cache.loadSDKSelectionPolicyOverrides(ctx, refs)
	policies := make(map[int]*fusedobject.PaginationConfig)
	// Policy projection stays in-memory only after the single set-based override lookup completes.
	for index, ref := range refs {
		effective := mergeExecutionPolicyOverride(metadata[index], overrides[ref])
		// Endpoint policy remains sufficient when the current effective service metadata has no fallback.
		if effective.Pagination == nil {
			continue
		}
		policy := *effective.Pagination
		policies[index] = &policy
	}
	return policies, nil
}

// mcpPaginationForSelections requires effective service metadata whenever the catalogue has selections.
func mcpPaginationForSelections(ctx context.Context, cache ObjectCache, selections []models.SDKSelection) (map[int]*fusedobject.PaginationConfig, error) {
	loader, ok := cache.(mcpPaginationLoader)
	// Empty catalogues need no policy capability and remain valid for transport-only adapters.
	if len(selections) == 0 {
		return map[int]*fusedobject.PaginationConfig{}, nil
	}
	// A selected service without effective metadata could be documented differently from execution.
	if !ok {
		return nil, errors.New("MCP pagination metadata lookup unavailable")
	}
	return loader.MCPPaginationForSelections(ctx, selections)
}

// fixturePagination reduces the effective policy to the public bound agents need for call().
func fixturePagination(endpoint, service *fusedobject.PaginationConfig) FixturePagination {
	policy := resolvePagination(endpoint, service)
	// An absent effective policy means execution-level pagination must be omitted.
	if policy == nil {
		return FixturePagination{Supported: false}
	}
	limits := paginationpolicy.EffectiveLimits((*paginationpolicy.Config)(policy).Limits)
	// Strict reduction leaves a legal positive caller bound only above one page.
	return FixturePagination{Supported: true, CallerBoundSupported: limits.MaxPages > 1, EngineMaxPages: limits.MaxPages}
}
