package accesscontrol

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

type workspacePlanAction struct {
	Type      string    `json:"type"`
	ServiceID uuid.UUID `json:"service_id"`
}

// WorkspacePlanApplyRequirements derives only plan-known apply authority.
// Optional credential material submitted later is authorized separately on
// the apply request because it is not part of the immutable plan.
func WorkspacePlanApplyRequirements(workspaceID uuid.UUID, raw json.RawMessage) ([]Requirement, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrInvalidRequirement
	}
	var actions []workspacePlanAction
	if err := json.Unmarshal(raw, &actions); err != nil {
		return nil, ErrInvalidRequirement
	}
	requirements := []Requirement{workspaceRequirement(workspaceID, PermissionWorkspaceUpdate)}
	for _, action := range actions {
		requirement, err := workspaceActionApplyRequirement(workspaceID, action)
		if err != nil {
			return nil, err
		}
		requirements = append(requirements, requirement)
	}
	return requirements, nil
}

func workspaceActionApplyRequirement(workspaceID uuid.UUID, action workspacePlanAction) (Requirement, error) {
	switch action.Type {
	case "add_service", "enable_service_version", "remove_service", "disable_service_version", "deprecate_service", "deprecate_version",
		"publish_service_execution_policy", "publish_service_version_execution_policy", "attach_connection_profile", "detach_connection_profile", "publish_connection_profile",
		"set_service_public", "set_service_private", "set_service_version_public", "set_service_version_private",
		"set_local_execution_policy", "reset_local_execution_policy", "set_local_service_version_execution_policy", "reset_local_service_version_execution_policy":
		if action.ServiceID == uuid.Nil {
			return Requirement{}, ErrInvalidRequirement
		}
		return Requirement{Permission: PermissionServiceManage, Resource: ResourceRef{Type: ResourceService, ID: action.ServiceID}}, nil
	case "create_bucket_binding":
		return workspaceRequirement(workspaceID, PermissionCredentialsManage), nil
	default:
		return Requirement{}, fmt.Errorf("%w: unsupported workspace action %q", ErrPolicyDenied, action.Type)
	}
}

func workspaceRequirement(workspaceID uuid.UUID, permission Permission) Requirement {
	return Requirement{Permission: permission, Resource: ResourceRef{Type: ResourceWorkspace, ID: workspaceID}}
}
