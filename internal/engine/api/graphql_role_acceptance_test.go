package api

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func TestGraphQLPolicyCataloguesBuiltInRoleContractMatrix(t *testing.T) {
	workspaceID := uuid.New()
	permissions := graphQLPolicyPermissionFamilies()
	if len(permissions) == 0 {
		t.Fatal("GraphQL policy catalogues contain no protected permission families")
	}
	for _, permission := range permissions {
		for _, role := range graphQLAcceptanceWorkspaceRoles() {
			t.Run(role+" "+string(permission), func(t *testing.T) {
				assertGraphQLRoleDecision(t, workspaceID, role, permission)
			})
		}
	}
}

func graphQLPolicyPermissionFamilies() []accesscontrol.Permission {
	unique := make(map[accesscontrol.Permission]struct{})
	collectEngineGraphQLPermissions(unique, engineGraphQLPolicy.queryRoots)
	collectEngineGraphQLPermissions(unique, engineGraphQLPolicy.mutationRoots)
	collectEngineGraphQLPermissions(unique, engineGraphQLPolicy.protected)
	collectRegistryGraphQLPermissions(unique, registryGraphQLQueryPolicies)
	collectRegistryGraphQLPermissions(unique, registryGraphQLMutationPolicies)
	permissions := make([]accesscontrol.Permission, 0, len(unique))
	for permission := range unique {
		permissions = append(permissions, permission)
	}
	sort.Slice(permissions, func(i, j int) bool { return permissions[i] < permissions[j] })
	return permissions
}

func collectEngineGraphQLPermissions(target map[accesscontrol.Permission]struct{}, policies map[string]graphQLFieldPolicy) {
	for _, policy := range policies {
		for _, permission := range policy.permissions {
			target[permission] = struct{}{}
		}
		if policy.relatedPermission != "" {
			target[policy.relatedPermission] = struct{}{}
		}
	}
}

func collectRegistryGraphQLPermissions(target map[accesscontrol.Permission]struct{}, policies map[string][]accesscontrol.Permission) {
	for _, permissions := range policies {
		for _, permission := range permissions {
			target[permission] = struct{}{}
		}
	}
}

func assertGraphQLRoleDecision(t *testing.T, workspaceID uuid.UUID, role string, permission accesscontrol.Permission) {
	t.Helper()
	actor := graphQLAcceptanceActor(t, workspaceID, role)
	plan := graphQLAuthorizationPlan{requirements: []accesscontrol.Requirement{{
		Permission: permission,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
	}}}
	downstreamCalls := 0
	err := authorizeGraphQLPlan(context.Background(), actor, plan)
	if err == nil {
		downstreamCalls++
	}
	wantAllowed := graphQLAcceptanceRoleHasPermission(role, permission)
	if wantAllowed && (err != nil || downstreamCalls != 1) {
		t.Fatalf("allowed error/calls = %v/%d, want nil/1", err, downstreamCalls)
	}
	if !wantAllowed && (err == nil || downstreamCalls != 0) {
		t.Fatalf("denied error/calls = %v/%d, want denial/0", err, downstreamCalls)
	}
}

func graphQLAcceptanceActor(t *testing.T, workspaceID uuid.UUID, role string) accesscontrol.Actor {
	t.Helper()
	for _, definition := range accesscontrol.BuiltInRoles() {
		if definition.Slug == role {
			return actorWithWorkspacePermissions(t, workspaceID, definition.Permissions...)
		}
	}
	t.Fatalf("built-in role %q not found", role)
	return accesscontrol.Actor{}
}

func graphQLAcceptanceRoleHasPermission(role string, permission accesscontrol.Permission) bool {
	for _, definition := range accesscontrol.BuiltInRoles() {
		if definition.Slug != role {
			continue
		}
		for _, candidate := range definition.Permissions {
			if candidate == permission {
				return true
			}
		}
		return false
	}
	return false
}

func graphQLAcceptanceWorkspaceRoles() []string {
	return []string{accesscontrol.RoleOwner, accesscontrol.RoleAdmin, accesscontrol.RoleBuilder, accesscontrol.RoleViewer}
}
