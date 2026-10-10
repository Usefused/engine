package cmd

import (
	"context"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// desiredAttachmentRequirements resolves exact hosted versions in one account-scoped batch before plan admission.
func (r *storeBackedControlRequirementResolver) desiredAttachmentRequirements(ctx context.Context, accountID uuid.UUID, kind store.ConfigType, references map[string]models.UnifiedAppReference) ([]accesscontrol.Requirement, error) {
	// Existing provider-only apps do not require attachment storage.
	if len(references) == 0 {
		return nil, nil
	}
	// All three execution adapters may attach apps; resolution stays bounded to the caller's account.
	if (kind != store.ConfigTypeSDK && kind != store.ConfigTypeMCP && kind != store.ConfigTypeUnifiedApp) || len(references) > 16 || accountID == uuid.Nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	repository, ok := r.store.(store.UnifiedAppAttachmentStore)
	// Missing storage is not evidence that no dependency permissions are needed.
	if !ok {
		return nil, accesscontrol.ErrPolicyDenied
	}
	bindings, err := repository.ResolveUnifiedAppBindings(ctx, accountID, references)
	// Partial or unavailable resolution cannot omit a requested capability from authorization.
	if err != nil || len(bindings) != len(references) {
		return nil, accesscontrol.ErrPolicyDenied
	}
	return attachedBindingRequirements(kind, bindings)
}

// attachedBindingRequirements retains typed use authority on each immutable dependency's family for plan and apply.
func attachedBindingRequirements(kind store.ConfigType, bindings []models.UnifiedAppBinding) ([]accesscontrol.Requirement, error) {
	// Provider-only consumers preserve their existing scope.
	if len(bindings) == 0 {
		return nil, nil
	}
	// Unsupported adapters and oversized scope cannot authorize a hosted dependency; runtime owns cycle checks.
	if (kind != store.ConfigTypeSDK && kind != store.ConfigTypeMCP && kind != store.ConfigTypeUnifiedApp) || len(bindings) > 16 {
		return nil, accesscontrol.ErrPolicyDenied
	}
	requirements := make([]accesscontrol.Requirement, 0, len(bindings))
	for _, binding := range bindings {
		// Both the immutable version and stable family must have resolved before granting use.
		if binding.AppID == uuid.Nil || binding.AppFamilyID == uuid.Nil {
			return nil, accesscontrol.ErrPolicyDenied
		}
		requirements = append(requirements, accesscontrol.Requirement{Permission: accesscontrol.PermissionAppUnifiedAppUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: binding.AppFamilyID}})
	}
	return requirements, nil
}
