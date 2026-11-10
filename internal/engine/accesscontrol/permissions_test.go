package accesscontrol

import (
	"errors"
	"testing"
)

// TestPermissionCatalogueContainsUniqueValidNames keeps the public scope catalogue explicit and free of retired broad aliases.
func TestPermissionCatalogueContainsUniqueValidNames(t *testing.T) {
	permissions := AllPermissions()
	seen := make(map[Permission]struct{}, len(permissions))
	for _, permission := range permissions {
		// Persisted grants must always belong to the explicit permission catalogue.
		if err := ValidatePermission(permission); err != nil {
			t.Fatalf("ValidatePermission(%q): %v", permission, err)
		}
		// A repeated grant would corrupt Owner's all-permissions projection.
		if _, duplicate := seen[permission]; duplicate {
			t.Fatalf("permission catalogue repeats %q", permission)
		}
		seen[permission] = struct{}{}
	}
	// Private webhook messages add a distinct grant without widening ordinary receipt permissions.
	if len(permissions) != 52 {
		t.Fatalf("permission catalogue contains %d entries, want 52", len(permissions))
	}
}

// TestWebhookDiagnosticsGrantIsExplicitAndServiceScoped prevents ordinary built-in roles inheriting body access.
func TestWebhookDiagnosticsGrantIsExplicitAndServiceScoped(t *testing.T) {
	for _, role := range BuiltInRoles() {
		// Owner retains its existing contract; all other roles require a separately authored diagnostic grant.
		if role.Slug != RoleOwner && roleHasPermission(role, PermissionWebhookDiagnosticsRead) {
			t.Fatalf("%s implicitly reveals webhook bodies", role.Slug)
		}
	}
	if !permissionSupportsRoleScope(PermissionWebhookDiagnosticsRead, ResourceService) || permissionSupportsRoleScope(PermissionWebhookDiagnosticsRead, ResourceApp) {
		// Webhook registrations are service resources, not Unified App families.
		t.Fatal("webhook diagnostics must bind to service scope")
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

// TestDiagnosticsPermissionIsExplicit prevents ordinary app read/manage roles from gaining raw-body access.
func TestDiagnosticsPermissionIsExplicit(t *testing.T) {
	for _, role := range BuiltInRoles() {
		for _, permission := range role.Permissions {
			// Owner retains its intentional all-permissions contract; every other built-in needs a separate grant.
			if permission == PermissionUnifiedAppDiagnosticsRead && role.Slug != RoleOwner {
				t.Fatalf("%s implicitly grants diagnostics", role.Slug)
			}
		}
	}
}
