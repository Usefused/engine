package accesscontrol

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
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
	serviceActions := []string{
		"add_service", "enable_service_version", "remove_service", "disable_service_version",
		"deprecate_service", "deprecate_version", "attach_connection_profile", "detach_connection_profile",
		"publish_connection_profile", "set_service_public", "set_service_private",
		"set_service_version_public", "set_service_version_private", "set_local_execution_policy",
		"reset_local_execution_policy", "set_local_service_version_execution_policy",
		"reset_local_service_version_execution_policy", "publish_service_execution_policy",
		"publish_service_version_execution_policy",
	}
	for _, actionType := range serviceActions {
		t.Run(actionType, func(t *testing.T) {
			actions, _ := json.Marshal([]map[string]any{{"type": actionType, "service_id": serviceID}})
			if _, err := WorkspacePlanApplyRequirements(workspaceID, actions); err != nil {
				t.Fatalf("action %q rejected: %v", actionType, err)
			}
		})
	}
}
