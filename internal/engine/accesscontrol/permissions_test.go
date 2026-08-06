package accesscontrol

import (
	"errors"
	"testing"
)

func TestPermissionCatalogueContainsUniqueValidNames(t *testing.T) {
	permissions := AllPermissions()
	seen := make(map[Permission]struct{}, len(permissions))
	for _, permission := range permissions {
		if err := ValidatePermission(permission); err != nil {
			t.Fatalf("ValidatePermission(%q): %v", permission, err)
		}
		if _, duplicate := seen[permission]; duplicate {
			t.Fatalf("permission catalogue repeats %q", permission)
		}
		seen[permission] = struct{}{}
	}
	if len(permissions) != 29 {
		t.Fatalf("permission catalogue contains %d entries, want 29", len(permissions))
	}
}

func TestValidatePermissionRejectsEmptyAndUnknownNames(t *testing.T) {
	for _, permission := range []Permission{"", "workspace.delete", " Workspace.read"} {
		if err := ValidatePermission(permission); !errors.Is(err, ErrInvalidPermission) {
			t.Fatalf("ValidatePermission(%q) error = %v, want ErrInvalidPermission", permission, err)
		}
	}
}

func TestAllPermissionsReturnsCopy(t *testing.T) {
	first := AllPermissions()
	first[0] = "changed"
	if second := AllPermissions(); second[0] != PermissionWorkspaceRead {
		t.Fatalf("AllPermissions exposed mutable catalogue: first entry = %q", second[0])
	}
}

func TestValidateResourceType(t *testing.T) {
	for _, resourceType := range []ResourceType{ResourceWorkspace, ResourceService, ResourceBucket, ResourceApp} {
		if err := ValidateResourceType(resourceType); err != nil {
			t.Fatalf("ValidateResourceType(%q): %v", resourceType, err)
		}
	}
	for _, resourceType := range []ResourceType{"", "team"} {
		if err := ValidateResourceType(resourceType); err == nil {
			t.Fatalf("ValidateResourceType(%q) unexpectedly succeeded", resourceType)
		}
	}
}
