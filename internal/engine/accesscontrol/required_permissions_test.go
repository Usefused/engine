package accesscontrol

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestRequiredPermissionsCanonicalRoundTrip(t *testing.T) {
	workspaceID := uuid.New()
	serviceID := uuid.New()
	requirements := []Requirement{
		{Permission: PermissionServiceRead, Resource: ResourceRef{Type: ResourceService, ID: serviceID}},
		{Permission: PermissionWorkspaceRead, Resource: ResourceRef{Type: ResourceWorkspace, ID: workspaceID}},
		{Permission: PermissionServiceRead, Resource: ResourceRef{Type: ResourceService, ID: serviceID}},
	}
	raw, err := MarshalRequiredPermissions(requirements)
	if err != nil {
		t.Fatalf("MarshalRequiredPermissions: %v", err)
	}
	decoded, err := UnmarshalRequiredPermissions(raw)
	if err != nil || len(decoded) != 2 {
		t.Fatalf("UnmarshalRequiredPermissions = %#v, %v", decoded, err)
	}
	if decoded[0].Permission != PermissionServiceRead || decoded[1].Permission != PermissionWorkspaceRead {
		t.Fatalf("canonical order = %#v", decoded)
	}
}

func TestRequiredPermissionsPreserveSafeDisplayNames(t *testing.T) {
	serviceID := uuid.New()
	resource := ResourceRef{Type: ResourceService, ID: serviceID}
	raw, err := MarshalRequiredPermissionsWithDisplayNames(
		[]Requirement{{Permission: PermissionServiceConsume, Resource: resource}},
		map[ResourceRef]string{resource: "Stripe"},
	)
	if err != nil {
		t.Fatalf("MarshalRequiredPermissionsWithDisplayNames: %v", err)
	}
	var serialized []RequiredPermission
	if err := json.Unmarshal(raw, &serialized); err != nil {
		t.Fatal(err)
	}
	if len(serialized) != 1 || serialized[0].DisplayName != "Stripe" {
		t.Fatalf("display name not preserved: %s", raw)
	}
	canonical, _, err := NormalizeRequiredPermissions(raw)
	if err != nil || string(canonical) != string(raw) {
		t.Fatalf("NormalizeRequiredPermissions = %s, %v", canonical, err)
	}
}

func TestRequiredPermissionsRejectUnsafeDisplayNames(t *testing.T) {
	resource := ResourceRef{Type: ResourceService, ID: uuid.New()}
	_, err := MarshalRequiredPermissionsWithDisplayNames(
		[]Requirement{{Permission: PermissionServiceRead, Resource: resource}},
		map[ResourceRef]string{resource: "unsafe\nname"},
	)
	if err == nil {
		t.Fatal("expected unsafe display name to be rejected")
	}
}

func TestRequiredPermissionsContextCopiesSlices(t *testing.T) {
	requirements := []Requirement{{Permission: PermissionWorkspaceRead, Resource: ResourceRef{Type: ResourceWorkspace, ID: uuid.New()}}}
	ctx := ContextWithRequiredPermissions(context.Background(), requirements)
	requirements[0].Permission = PermissionWorkspaceUpdate
	got, ok := RequiredPermissionsFromContext(ctx)
	if !ok || got[0].Permission != PermissionWorkspaceRead {
		t.Fatalf("context requirements = %#v, %v", got, ok)
	}
	got[0].Permission = PermissionWorkspaceUpdate
	again, _ := RequiredPermissionsFromContext(ctx)
	if again[0].Permission != PermissionWorkspaceRead {
		t.Fatalf("context slice was mutable: %#v", again)
	}
}

func TestRequiredPermissionsCapturePublishesDownstreamResolution(t *testing.T) {
	ctx, capture := ContextWithRequiredPermissionsCapture(context.Background())
	requirements := []Requirement{{Permission: PermissionAccessManage, Resource: ResourceRef{Type: ResourceWorkspace, ID: uuid.New()}}}
	if !CaptureRequiredPermissions(ctx, requirements) {
		t.Fatal("capture was not found in context")
	}
	if !CaptureMissingPermissions(ctx, requirements) {
		t.Fatal("missing-permission capture was not found in context")
	}
	requirements[0].Permission = PermissionWorkspaceRead
	got, ok := capture.RequiredPermissions()
	if !ok || got[0].Permission != PermissionAccessManage {
		t.Fatalf("captured requirements = %#v, %v", got, ok)
	}
	fromContext, ok := RequiredPermissionsFromContext(ctx)
	if !ok || fromContext[0] != got[0] {
		t.Fatalf("context requirements = %#v, %v", fromContext, ok)
	}
	missing, ok := MissingPermissionsFromContext(ctx)
	if !ok || len(missing) != 1 || missing[0].Permission != PermissionAccessManage {
		t.Fatalf("missing requirements = %#v, %v", missing, ok)
	}
}
