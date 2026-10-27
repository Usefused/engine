package cmd

import (
	"context"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// hasDesiredExecutionScope admits local attachments without weakening the rejection of empty apps.
func hasDesiredExecutionScope(services, hosted, bridges int) bool {
	return services+hosted+bridges > 0
}

// desiredAppAttachmentRequirements checks delegation authority before handlers resolve exact Bridge contracts.
func (r *storeBackedControlRequirementResolver) desiredAppAttachmentRequirements(ctx context.Context, actor accesscontrol.Actor, kind store.ConfigType, hosted map[string]models.UnifiedAppReference, bridgeCount int) ([]accesscontrol.Requirement, error) {
	requirements, err := r.desiredAttachmentRequirements(ctx, actor.AccountID, kind, hosted)
	// A local attachment must never conceal a failed hosted dependency.
	if err != nil {
		return nil, err
	}
	bridges, err := bridgeWorkspaceRequirements(kind, actor.WorkspaceID, bridgeCount)
	return append(requirements, bridges...), err
}

// bridgeWorkspaceRequirements preserves workspace sharing authority for every supported app adapter.
func bridgeWorkspaceRequirements(kind store.ConfigType, workspaceID uuid.UUID, count int) ([]accesscontrol.Requirement, error) {
	// Provider-only apps keep their existing permission requirements.
	if count == 0 {
		return nil, nil
	}
	// The authorization boundary enforces the same finite adapter scope as contract admission.
	if count > 100 || workspaceID == uuid.Nil {
		return nil, accesscontrol.ErrPolicyDenied
	}
	// Webhook ingress cannot delegate outbound local capabilities.
	switch kind {
	case store.ConfigTypeSDK, store.ConfigTypeMCP, store.ConfigTypeUnifiedApp:
		return []accesscontrol.Requirement{workspaceAccessRequirement(workspaceID, accesscontrol.PermissionBridgeManage)}, nil
	default:
		return nil, accesscontrol.ErrPolicyDenied
	}
}

// storedAppAttachmentRequirements projects server-resolved identities rather than trusting new apply input.
func storedAppAttachmentRequirements(kind store.ConfigType, hosted []models.UnifiedAppBinding, bridges []models.BridgeBinding) ([]accesscontrol.Requirement, error) {
	requirements, err := attachedBindingRequirements(kind, hosted)
	// Hosted identity validation remains mandatory for mixed plans.
	if err != nil {
		return nil, err
	}
	seen := make(map[uuid.UUID]bool)
	for _, binding := range bridges {
		workspaceID, err := uuid.Parse(binding.WorkspaceID)
		// Missing workspace identity cannot be converted into a broad delegation grant.
		if err != nil {
			return nil, accesscontrol.ErrPolicyDenied
		}
		scope, err := bridgeWorkspaceRequirements(kind, workspaceID, len(bridges))
		// Bounds and adapter checks apply before deduplicating any workspace grant.
		if err != nil {
			return nil, err
		}
		// Multiple operations from one workspace share the same required delegation permission.
		if !seen[workspaceID] {
			requirements = append(requirements, scope...)
			seen[workspaceID] = true
		}
	}
	return requirements, nil
}
