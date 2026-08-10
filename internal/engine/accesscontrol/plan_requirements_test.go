package accesscontrol

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/workspaceplan"
)

func TestWorkspacePlanApplyRequirements(t *testing.T) {
	workspaceID := uuid.New()
	serviceID := uuid.New()
	actions, err := json.Marshal([]map[string]any{
		{"type": "set_service_public", "service_id": serviceID},
		{"type": "create_bucket_binding"},
		{"type": "set_service_public", "service_id": serviceID},
	})
	if err != nil {
		t.Fatal(err)
	}

	requirements, err := WorkspacePlanApplyRequirements(workspaceID, actions)
	if err != nil {
		t.Fatalf("WorkspacePlanApplyRequirements: %v", err)
	}
	raw, err := MarshalRequiredPermissions(requirements)
	if err != nil {
		t.Fatalf("MarshalRequiredPermissions: %v", err)
	}
	want := `[{"permission":"credentials.manage","resource_type":"workspace","resource_id":"` + workspaceID.String() + `"},{"permission":"service.manage","resource_type":"service","resource_id":"` + serviceID.String() + `"},{"permission":"workspace.update","resource_type":"workspace","resource_id":"` + workspaceID.String() + `"}]`
	if string(raw) != want {
		t.Fatalf("required permissions = %s, want %s", raw, want)
	}
}

func TestWorkspacePlanApplyRequirementsFailsClosed(t *testing.T) {
	_, err := WorkspacePlanApplyRequirements(uuid.New(), json.RawMessage(`[{"type":"unknown"}]`))
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("expected ErrPolicyDenied, got %v", err)
	}
}

func TestWorkspacePlanApplyRequirementsCoversPlannedActionTypes(t *testing.T) {
	workspaceID := uuid.New()
	serviceID := uuid.New()
	for _, actionType := range workspaceplan.ActionTypes() {
		t.Run(actionType.String(), func(t *testing.T) {
			actions, _ := json.Marshal([]map[string]any{{"type": actionType, "service_id": serviceID}})
			if _, err := WorkspacePlanApplyRequirements(workspaceID, actions); err != nil {
				t.Fatalf("action %q rejected: %v", actionType, err)
			}
		})
	}
}
