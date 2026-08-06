package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

var acceptanceDynamicPermissions = map[dynamicRequirementKind][]accesscontrol.Permission{
	dynamicServiceCreate:      {accesscontrol.PermissionServiceManage},
	dynamicBucketByName:       {accesscontrol.PermissionBucketManage},
	dynamicSecretWrite:        {accesscontrol.PermissionCredentialsManage},
	dynamicWorkspaceApply:     {accesscontrol.PermissionWorkspaceRead, accesscontrol.PermissionServiceManage, accesscontrol.PermissionBucketManage, accesscontrol.PermissionCredentialsManage},
	dynamicConfigPlanAction:   {accesscontrol.PermissionWorkspaceUpdate},
	dynamicWorkspacePlan:      {accesscontrol.PermissionWorkspaceRead, accesscontrol.PermissionServiceRead, accesscontrol.PermissionBucketRead},
	dynamicDesiredConfigPlan:  {accesscontrol.PermissionAppCreate, accesscontrol.PermissionServiceRead, accesscontrol.PermissionBucketRead},
	dynamicDesiredConfigApply: {accesscontrol.PermissionAppCreate, accesscontrol.PermissionServiceConsume, accesscontrol.PermissionBucketUse},
	dynamicSDKGenerate:        {accesscontrol.PermissionAppCreate, accesscontrol.PermissionServiceConsume, accesscontrol.PermissionBucketUse},
	dynamicAppAccess:          {accesscontrol.PermissionAppManage, accesscontrol.PermissionAppRead},
	dynamicAppTokenAccess:     {accesscontrol.PermissionAppTokensManage},
}

type acceptanceRequirementResolver struct {
	workspaceID uuid.UUID
}

func (resolver acceptanceRequirementResolver) ResolveControlRequirements(_ context.Context, _ accesscontrol.Actor, kind dynamicRequirementKind, _ map[string]string, _ *http.Request) ([]accesscontrol.Requirement, error) {
	permissions := acceptanceDynamicPermissions[kind]
	requirements := make([]accesscontrol.Requirement, 0, len(permissions))
	for _, permission := range permissions {
		requirements = append(requirements, workspaceAccessRequirement(resolver.workspaceID, permission))
	}
	return requirements, nil
}

func TestControlRESTManifestBuiltInRoleContractMatrix(t *testing.T) {
	assertAcceptanceDynamicKindsCovered(t)
	workspaceID := uuid.New()
	resolver := acceptanceRequirementResolver{workspaceID: workspaceID}
	for _, policy := range controlRESTPolicies {
		requirements := acceptancePolicyRequirements(t, policy, workspaceID, resolver)
		for _, role := range acceptanceWorkspaceRoles() {
			t.Run(role+" "+policy.method+" "+policy.pattern, func(t *testing.T) {
				assertRESTPolicyRoleDecision(t, policy, workspaceID, resolver, requirements, role)
			})
		}
	}
}

func assertAcceptanceDynamicKindsCovered(t *testing.T) {
	t.Helper()
	referenced := make(map[dynamicRequirementKind]struct{})
	for _, kind := range dynamicControlRequirements {
		referenced[kind] = struct{}{}
		if len(acceptanceDynamicPermissions[kind]) == 0 {
			t.Errorf("dynamic policy kind %q has no role-matrix permission family", kind)
		}
	}
	for kind := range acceptanceDynamicPermissions {
		if _, ok := referenced[kind]; !ok {
			t.Errorf("stale role-matrix dynamic policy kind %q", kind)
		}
	}
}

func acceptancePolicyRequirements(t *testing.T, policy controlRoutePolicy, workspaceID uuid.UUID, resolver controlRequirementResolver) []accesscontrol.Requirement {
	t.Helper()
	path, _ := concretePolicyPath(policy.pattern)
	request := requestWithActor(t, policy.method, path, actorWithGrants(t, workspaceID))
	request.URL.RawQuery = policyQuery(policy).Encode()
	requirements, _, ok := resolveControlRESTPolicy(request, resolver)
	if !ok {
		t.Fatalf("policy did not resolve: %s %s", policy.method, policy.pattern)
	}
	return requirements
}

func assertRESTPolicyRoleDecision(t *testing.T, policy controlRoutePolicy, workspaceID uuid.UUID, resolver controlRequirementResolver, requirements []accesscontrol.Requirement, role string) {
	t.Helper()
	downstreamCalls := 0
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{}, resolver)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	path, _ := concretePolicyPath(policy.pattern)
	request := requestWithActor(t, policy.method, path, workspaceRoleActor(t, workspaceID, role))
	request.URL.RawQuery = policyQuery(policy).Encode()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	wantAllowed := acceptanceRoleAllowsAll(role, requirements)
	if wantAllowed && (response.Code != http.StatusNoContent || downstreamCalls != 1) {
		t.Fatalf("allowed status/calls = %d/%d, want 204/1", response.Code, downstreamCalls)
	}
	if !wantAllowed && (response.Code != http.StatusForbidden || downstreamCalls != 0) {
		t.Fatalf("denied status/calls = %d/%d, want 403/0", response.Code, downstreamCalls)
	}
}

func acceptanceRoleAllowsAll(role string, requirements []accesscontrol.Requirement) bool {
	for _, requirement := range requirements {
		if !acceptanceRoleAllowsPermission(role, requirement.Permission) {
			return false
		}
	}
	return true
}

func acceptanceRoleAllowsPermission(role string, permission accesscontrol.Permission) bool {
	switch role {
	case accesscontrol.RoleOwner:
		return true
	case accesscontrol.RoleAdmin:
		return permission != accesscontrol.PermissionAccountManage && permission != accesscontrol.PermissionBillingManage
	case accesscontrol.RoleBuilder:
		return acceptanceBuilderPermission(permission)
	case accesscontrol.RoleViewer:
		return acceptanceViewerPermission(permission)
	default:
		return false
	}
}

func acceptanceBuilderPermission(permission accesscontrol.Permission) bool {
	switch permission {
	case accesscontrol.PermissionWorkspaceRead,
		accesscontrol.PermissionAppCreate,
		accesscontrol.PermissionCatalogueRead,
		accesscontrol.PermissionAccountRead,
		accesscontrol.PermissionBillingRead,
		accesscontrol.PermissionNotificationUpdate:
		return true
	default:
		return false
	}
}

func acceptanceViewerPermission(permission accesscontrol.Permission) bool {
	switch permission {
	case accesscontrol.PermissionWorkspaceRead,
		accesscontrol.PermissionCatalogueRead,
		accesscontrol.PermissionAccountRead,
		accesscontrol.PermissionBillingRead:
		return true
	default:
		return false
	}
}

func acceptanceWorkspaceRoles() []string {
	return []string{accesscontrol.RoleOwner, accesscontrol.RoleAdmin, accesscontrol.RoleBuilder, accesscontrol.RoleViewer}
}
