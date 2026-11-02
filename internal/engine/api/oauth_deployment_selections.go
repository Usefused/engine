package api

import (
	"context"
	"encoding/json"

	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// validateDeploymentPlan binds a later publication to the original approved immutable provider objects.
func validateDeploymentPlan(ctx context.Context, repository store.Store, account uuid.UUID, payload appResolvedPayload, ceiling *store.OAuthRuntimeGrant) error {
	// Initial browser consent establishes the ceiling; later deployments must prove it before apply.
	if ceiling == nil {
		return nil
	}
	original, err := repository.GetApp(ctx, ceiling.AppID)
	// Names are descriptive only; the persisted app and family IDs own the approved selection snapshot.
	if err != nil || original == nil || original.AppID != ceiling.AppID || original.AppFamilyID != ceiling.AppFamilyID || original.AccountID != account {
		return oauthprovider.ErrInvalidScope
	}
	var approved []models.SDKSelection
	// Missing historical contracts require fresh browser review, never a current-catalogue substitute.
	if json.Unmarshal(original.Selections, &approved) != nil || !deploymentBindingsWithin(allAppContractBindings(payload), ceiling.ContractBindings) {
		return oauthprovider.ErrInvalidScope
	}
	resolver, ok := repository.(store.GenerationSelectionResolver)
	// A deployment must resolve concrete endpoint IDs before any version can be published.
	if !ok || len(payload.Bridges) != 0 || len(payload.UnifiedApps) != 0 {
		return oauthprovider.ErrInvalidScope
	}
	planned := finalizeAppSelections(append([]models.SDKSelection(nil), payload.Selections...), payload.ContractBindings)
	resolved, err := resolver.ResolveGenerationSelections(ctx, planned)
	// An operation label reused for another immutable endpoint cannot inherit prior consent.
	if err != nil || len(resolved) != len(planned) || !deploymentSelectionsWithin(resolved, approved) {
		return oauthprovider.ErrInvalidScope
	}
	return nil
}

// deploymentBindingsWithin compares source/runtime hashes as well as version IDs, including hidden credential sources.
func deploymentBindingsWithin(next, approved []models.SDKContractBinding) bool {
	type identity struct{ service, version uuid.UUID }
	allowed := make(map[identity]models.SDKContractBinding, len(approved))
	// An absent hash ceiling would allow a refreshed snapshot under unchanged human-readable labels.
	if len(next) == 0 || len(approved) == 0 {
		return false
	}
	for _, binding := range approved {
		key := identity{binding.ServiceID, binding.ServiceVersionID}
		// Duplicate or unpinned historical bindings cannot establish unique authority.
		if _, duplicate := allowed[key]; duplicate || binding.ServiceID == uuid.Nil || binding.ServiceVersionID == uuid.Nil || binding.SourceHash == "" || binding.GenerationContractHash == "" && binding.RuntimeContractHash == "" {
			return false
		}
		allowed[key] = binding
	}
	seen := make(map[identity]bool, len(next))
	for _, binding := range next {
		key := identity{binding.ServiceID, binding.ServiceVersionID}
		previous, exists := allowed[key]
		// Revision, schema/content hash and immutable IDs must all remain exactly as approved.
		if !exists || seen[key] || previous != binding {
			return false
		}
		seen[key] = true
	}
	return true
}

// deploymentSelectionsWithin admits subsets only when every selected immutable endpoint was originally approved.
func deploymentSelectionsWithin(next, approved []models.SDKSelection) bool {
	type identity struct{ service, version uuid.UUID }
	allowed := make(map[identity]map[uuid.UUID]bool, len(approved))
	// Runtime grants always contain at least one explicit physical operation.
	if len(next) == 0 || len(approved) == 0 {
		return false
	}
	for _, selection := range approved {
		key := identity{selection.ServiceID, selection.ServiceVersionID}
		// Historical wildcard or nonphysical scopes are not valid deployment ceilings.
		if selection.ServiceID == uuid.Nil || selection.ServiceVersionID == uuid.Nil || selection.SelectAll || selection.WebhookSelectAll || len(selection.WebhookIDs) != 0 || len(selection.EndpointIDs) == 0 || selection.ImportedMCP != nil {
			return false
		}
		// Two aliases may legitimately refer to the same provider; their reviewed endpoint union is the ceiling.
		if allowed[key] == nil {
			allowed[key] = make(map[uuid.UUID]bool)
		}
		for _, endpoint := range selection.EndpointIDs {
			// Empty endpoint identities cannot authorize a future operation.
			if endpoint == uuid.Nil {
				return false
			}
			allowed[key][endpoint] = true
		}
	}
	for _, selection := range next {
		endpoints := allowed[identity{selection.ServiceID, selection.ServiceVersionID}]
		// Even matching operation names cannot replace the resolved provider revision or executable ID.
		if len(endpoints) == 0 || selection.SelectAll || selection.WebhookSelectAll || len(selection.WebhookIDs) != 0 || len(selection.EndpointIDs) == 0 || selection.ImportedMCP != nil {
			return false
		}
		for _, endpoint := range selection.EndpointIDs {
			// Lookup requires the exact initially approved endpoint identity.
			if !endpoints[endpoint] {
				return false
			}
		}
	}
	return true
}
