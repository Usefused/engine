package accesscontrol

import (
	"errors"
	"testing"
)

func TestBuiltInRolesAreValidAndUnique(t *testing.T) {
	seen := make(map[string]struct{})
	for _, role := range BuiltInRoles() {
		if err := ValidateRoleDefinition(role); err != nil {
			t.Fatalf("ValidateRoleDefinition(%q): %v", role.Slug, err)
		}
		if _, duplicate := seen[role.Slug]; duplicate {
			t.Fatalf("built-in role slug %q is duplicated", role.Slug)
		}
		seen[role.Slug] = struct{}{}
	}
}

func TestBuiltInRolesReturnsDeepCopy(t *testing.T) {
	roles := BuiltInRoles()
	roles[0].Slug = "changed"
	roles[0].Permissions[0] = "changed"

	fresh := BuiltInRoles()
	if fresh[0].Slug != RoleOwner || fresh[0].Permissions[0] != PermissionWorkspaceRead {
		t.Fatalf("BuiltInRoles exposed mutable definitions: %#v", fresh[0])
	}
}

func TestOwnerContainsEveryPermission(t *testing.T) {
	owner := roleBySlug(t, RoleOwner)
	if len(owner.Permissions) != len(AllPermissions()) {
		t.Fatalf("owner has %d permissions, want %d", len(owner.Permissions), len(AllPermissions()))
	}
	for _, permission := range AllPermissions() {
		if !roleHasPermission(owner, permission) {
			t.Fatalf("owner is missing %q", permission)
		}
	}
}

func TestWorkspaceRolesDoNotImplicitlyGrantScopedResourceUse(t *testing.T) {
	for _, slug := range []string{RoleBuilder, RoleViewer} {
		role := roleBySlug(t, slug)
		for _, permission := range []Permission{PermissionServiceConsume, PermissionBucketUse} {
			if roleHasPermission(role, permission) {
				t.Fatalf("workspace role %q must receive %q through a resource binding", slug, permission)
			}
		}
	}
}

func TestAdminCannotManageAccountOrBilling(t *testing.T) {
	admin := roleBySlug(t, RoleAdmin)
	for _, permission := range []Permission{PermissionAccountManage, PermissionBillingManage} {
		if roleHasPermission(admin, permission) {
			t.Fatalf("admin unexpectedly contains owner-only permission %q", permission)
		}
	}
}

func TestWorkspaceShareRolesCannotManageSharedResources(t *testing.T) {
	tests := []struct {
		role      string
		allows    Permission
		forbidden []Permission
	}{
		{role: RoleBucketUser, allows: PermissionBucketUse, forbidden: []Permission{PermissionBucketManage, PermissionCredentialsManage}},
		{role: RoleAppUser, allows: PermissionAppUse, forbidden: []Permission{PermissionAppManage, PermissionAppTokensManage}},
	}
	for _, test := range tests {
		role := roleBySlug(t, test.role)
		if !roleHasPermission(role, test.allows) {
			t.Fatalf("workspace share role %q is missing %q", test.role, test.allows)
		}
		for _, permission := range test.forbidden {
			if roleHasPermission(role, permission) {
				t.Fatalf("workspace share role %q unexpectedly grants %q", test.role, permission)
			}
		}
	}
}

func TestValidateRoleDefinitionRejectsInvalidDefinitions(t *testing.T) {
	tests := []RoleDefinition{
		{},
		{Slug: "empty", DisplayName: "Empty", ScopeType: ResourceWorkspace},
		{Slug: "bad-scope", DisplayName: "Bad scope", ScopeType: "team", Permissions: []Permission{PermissionAccessRead}},
		{Slug: "bad-permission", DisplayName: "Bad permission", ScopeType: ResourceWorkspace, Permissions: []Permission{"unknown"}},
		{Slug: "duplicate", DisplayName: "Duplicate", ScopeType: ResourceWorkspace, Permissions: []Permission{PermissionWorkspaceRead, PermissionWorkspaceRead}},
	}
	for _, role := range tests {
		if err := ValidateRoleDefinition(role); !errors.Is(err, ErrInvalidRoleDefinition) {
			t.Fatalf("ValidateRoleDefinition(%q) error = %v, want ErrInvalidRoleDefinition", role.Slug, err)
		}
	}
}

func TestValidateRoleDefinitionRejectsPermissionAtWrongResourceScope(t *testing.T) {
	role := RoleDefinition{
		Slug:        "invalid-service-role",
		DisplayName: "Invalid service role",
		ScopeType:   ResourceService,
		Permissions: []Permission{PermissionWorkspaceRead},
	}
	if err := ValidateRoleDefinition(role); !errors.Is(err, ErrInvalidRoleDefinition) {
		t.Fatalf("error = %v, want ErrInvalidRoleDefinition", err)
	}
}

func roleBySlug(t *testing.T, slug string) RoleDefinition {
	t.Helper()
	for _, role := range BuiltInRoles() {
		if role.Slug == slug {
			return role
		}
	}
	t.Fatalf("built-in role %q not found", slug)
	return RoleDefinition{}
}

func roleHasPermission(role RoleDefinition, permission Permission) bool {
	for _, candidate := range role.Permissions {
		if candidate == permission {
			return true
		}
	}
	return false
}
