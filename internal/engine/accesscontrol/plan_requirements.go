package accesscontrol

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/workspaceplan"
)

type workspacePlanAction struct {
	Type      workspaceplan.ActionType `json:"type"`
	ServiceID uuid.UUID                `json:"service_id"`
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
	class, valid := action.Type.AuthorizationClass()
	if !valid {
		return Requirement{}, fmt.Errorf("%w: unsupported workspace action %q", ErrPolicyDenied, action.Type)
	}
	switch class {
	case workspaceplan.AuthorizationServiceManage:
		if action.ServiceID == uuid.Nil {
			return Requirement{}, ErrInvalidRequirement
		}
		return Requirement{Permission: PermissionServiceManage, Resource: ResourceRef{Type: ResourceService, ID: action.ServiceID}}, nil
	case workspaceplan.AuthorizationCredentialsManage:
		return workspaceRequirement(workspaceID, PermissionCredentialsManage), nil
	default:
		return Requirement{}, ErrInvalidRequirement
	}
}

func workspaceRequirement(workspaceID uuid.UUID, permission Permission) Requirement {
	return Requirement{Permission: permission, Resource: ResourceRef{Type: ResourceWorkspace, ID: workspaceID}}
}
