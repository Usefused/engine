package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

func TestControlRESTPolicyManifestIsValid(t *testing.T) {
	if err := validateControlRESTPolicies(); err != nil {
		t.Fatal(err)
	}
}

// TestDiscoveryV1ControlRoutesReplaceLegacyMutationSurface keeps Engine's
// fail-closed proxy policy synchronized with Registry's breaking protocol.
func TestDiscoveryV1ControlRoutesReplaceLegacyMutationSurface(t *testing.T) {
	current := map[string]accesscontrol.Permission{
		http.MethodPost + " /integrations/session/{session_id}/actions":       accesscontrol.PermissionCatalogueImport,
		http.MethodGet + " /integrations/session/{session_id}/review-summary": accesscontrol.PermissionCatalogueRead,
	}
	manifest := make(map[string]controlRoutePolicy, len(controlRESTPolicies))
	for _, policy := range controlRESTPolicies {
		manifest[policy.method+" "+policy.pattern] = policy
	}
	for route, permission := range current {
		policy, found := manifest[route]
		if !found || len(policy.requirements) != 1 || policy.requirements[0].permission != permission {
			t.Fatalf("discovery policy %s = %#v, want workspace %s", route, policy.requirements, permission)
		}
	}
	for _, retired := range []string{
		http.MethodPost + " /integrations/respond",
		http.MethodPost + " /integrations/session/{session_id}/recover",
		http.MethodPost + " /integrations/session/{session_id}/cancel",
		http.MethodDelete + " /integrations/session/{session_id}",
	} {
		if _, found := manifest[retired]; found {
			t.Fatalf("retired discovery policy remains authorized: %s", retired)
		}
	}
}

func TestAuthenticatedIdentityRoutesDoNotRequireWorkspaceGrants(t *testing.T) {
	workspaceID := uuid.New()
	actor := actorWithGrants(t, workspaceID)
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/auth/whoami"},
		{http.MethodPost, "/auth/cli/logout"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			called := false
			handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, requestWithActor(t, route.method, route.path, actor))
			if response.Code != http.StatusNoContent || !called {
				t.Fatalf("authenticated-only response = %d, called=%v", response.Code, called)
			}
		})
	}
}

func TestControlRESTPolicyManifestCoversNativeRoutes(t *testing.T) {
	workspaceID := uuid.New()
	actor := actorWithGrants(t, workspaceID)
	router := chi.NewRouter()
	registerNativeRESTControlRoutes(router, engineRouterDeps{})
	resolver := fixedControlRequirementResolver{requirement: workspaceAccessRequirement(workspaceID, accesscontrol.PermissionWorkspaceRead)}

	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		path := sampleControlRoutePath(route)
		request := requestWithActor(t, method, path, actor)
		if route == workspaceAppTokensPath {
			query := request.URL.Query()
			query.Set("app_family_id", uuid.NewString())
			request.URL.RawQuery = query.Encode()
		}
		if classifyEngineRequest(request) == requestClassRuntimeExcluded {
			return nil
		}
		if _, _, ok := resolveControlRESTPolicy(request, resolver); !ok {
			t.Errorf("registered route has no authorization policy: %s %s", method, route)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk native routes: %v", err)
	}
}

var routeParameterPattern = regexp.MustCompile(`\{[^}]+\}`)

func sampleControlRoutePath(pattern string) string {
	return routeParameterPattern.ReplaceAllString(pattern, uuid.NewString())
}

type fixedControlRequirementResolver struct {
	requirement accesscontrol.Requirement
}

// Unit tests outside the audit-specific suite use a validating recorder so
// they exercise authorization without weakening production's nil-recorder
// fail-closed rule for mutations.
func controlAuthorizationMiddleware(authorizer accesscontrol.Authorizer, resolvers ...controlRequirementResolver) func(http.Handler) http.Handler {
	return controlAuthorizationMiddlewareWithAudit(authorizer, firstControlRequirementResolver(resolvers), &controlAuditRecorderStub{})
}

func (r fixedControlRequirementResolver) ResolveControlRequirements(context.Context, accesscontrol.Actor, dynamicRequirementKind, map[string]string, *http.Request) ([]accesscontrol.Requirement, error) {
	return []accesscontrol.Requirement{r.requirement}, nil
}

func TestControlAuthorizationUsesResourceIDsFromPath(t *testing.T) {
	workspaceID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	actor := actorWithGrants(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionCredentialsManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
	)
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	allowedPath := "/workspace/buckets/" + bucketID.String() + "/services/" + serviceID.String() + "/connect-config"
	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, requestWithActor(t, http.MethodPut, allowedPath, actor))
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("allowed status = %d; body=%s", allowed.Code, allowed.Body.String())
	}
	if !strings.Contains(allowed.Header().Get("Server-Timing"), "engine_authz;dur=") {
		t.Fatalf("Server-Timing = %q", allowed.Header().Get("Server-Timing"))
	}

	deniedPath := "/workspace/buckets/" + uuid.NewString() + "/services/" + serviceID.String() + "/connect-config"
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, requestWithActor(t, http.MethodPut, deniedPath, actor))
	assertControlDenial(t, denied, http.StatusForbidden, "permission_denied", 1)
}

func TestControlAuthorizationRequiresEveryDeclaredPermission(t *testing.T) {
	workspaceID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	actor := actorWithGrants(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionCredentialsManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}},
	)
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not execute without service.consume")
	}))

	response := httptest.NewRecorder()
	path := "/workspace/buckets/" + bucketID.String() + "/services/" + serviceID.String() + "/connect-config"
	handler.ServeHTTP(response, requestWithActor(t, http.MethodPut, path, actor))
	assertControlDenial(t, response, http.StatusForbidden, "permission_denied", 1)
}

func TestControlAuthorizationRemovingEitherPermissionHasNoSideEffect(t *testing.T) {
	workspaceID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	all := []accesscontrol.Grant{
		{Permission: accesscontrol.PermissionCredentialsManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}},
		{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
	}
	for removed := range all {
		t.Run(string(all[removed].Permission), func(t *testing.T) {
			grants := append([]accesscontrol.Grant(nil), all[:removed]...)
			grants = append(grants, all[removed+1:]...)
			actor := actorWithGrants(t, workspaceID, grants...)
			called := false
			handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}))
			path := "/workspace/buckets/" + bucketID.String() + "/services/" + serviceID.String() + "/connect-config"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, requestWithActor(t, http.MethodPut, path, actor))
			if response.Code != http.StatusForbidden || called {
				t.Fatalf("status/called = %d/%v, want 403/false", response.Code, called)
			}
		})
	}
}

func TestControlAuthorizationDefaultDeniesUnknownControlRoutes(t *testing.T) {
	workspaceID := uuid.New()
	actor := ownerActor(t, workspaceID)
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/workspace/unclassified"},
		{http.MethodGet, "/integrations/future-route"},
		// Registry deletion must go through Engine's /sdk-config lifecycle so
		// local config/runtime state cannot be left pointing at a retired SDK.
		{http.MethodDelete, "/sdks/" + uuid.NewString()},
	} {
		t.Run(request.method+" "+request.path, func(t *testing.T) {
			handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("unknown control route must not execute")
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, requestWithActor(t, request.method, request.path, actor))
			assertControlDenial(t, response, http.StatusForbidden, "permission_denied", 0)
		})
	}
}

func TestAuditExportRouteDeniesWithoutAuditReadBeforeHandler(t *testing.T) {
	workspaceID := uuid.New()
	actor := actorWithGrants(t, workspaceID, accesscontrol.Grant{
		Permission: accesscontrol.PermissionAccessRead,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
	})
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("audit export must not execute without audit.read")
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, requestWithActor(t, http.MethodGet, "/audit/export", actor))
	assertControlDenial(t, response, http.StatusForbidden, "permission_denied", 1)
}

func TestControlAuthorizationReturns401WithoutCachedActor(t *testing.T) {
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unauthenticated control route must not execute")
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/integrations", nil))
	assertControlDenial(t, response, http.StatusUnauthorized, "authentication_required", 0)
}

func TestControlAuthorizationPreservesRuntimePublicAndGraphQLBoundaries(t *testing.T) {
	called := 0
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	}))
	paths := []string{
		"/health",
		"/mcp/server",
		"/mcp/server/sse",
		"/webhook/example",
		"/workspace/connect/callback",
		"/workspace/connect/input",
		"/graphql",
		"/engine/graphql",
	}
	for _, path := range paths {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("path %s status = %d", path, response.Code)
		}
	}
	if called != len(paths) {
		t.Fatalf("handler calls = %d, want %d", called, len(paths))
	}
}

// TestConnectBrandingRoutesRequireWorkspacePermissions locks the settings read
// and mutation to their distinct least-privilege workspace capabilities.
func TestConnectBrandingRoutesRequireWorkspacePermissions(t *testing.T) {
	wants := map[string]accesscontrol.Permission{
		http.MethodGet + " /workspace/connect-branding": accesscontrol.PermissionWorkspaceRead,
		http.MethodPut + " /workspace/connect-branding": accesscontrol.PermissionWorkspaceUpdate,
	}
	for key, want := range wants {
		found := false
		for _, policy := range controlRESTPolicies {
			if policy.method+" "+policy.pattern != key {
				continue
			}
			found = true
			if len(policy.requirements) != 1 || policy.requirements[0].permission != want || policy.requirements[0].resourceType != accesscontrol.ResourceWorkspace {
				t.Fatalf("branding policy %s = %#v, want workspace %s", key, policy.requirements, want)
			}
		}
		if !found {
			t.Fatalf("branding policy %s is missing", key)
		}
	}
}

func TestControlAuthorizationRejectsMalformedScopedResource(t *testing.T) {
	workspaceID := uuid.New()
	response := httptest.NewRecorder()
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("malformed scoped route must not execute")
	}))
	handler.ServeHTTP(response, requestWithActor(t, http.MethodPut, "/workspace/buckets/not-a-uuid/values", ownerActor(t, workspaceID)))
	assertControlDenial(t, response, http.StatusForbidden, "permission_denied", 0)
}

// TestControlEndpointRoleMatrix pins representative role boundaries for control-plane routes.
func TestControlEndpointRoleMatrix(t *testing.T) {
	workspaceID := uuid.New()
	tests := []struct {
		method  string
		path    string
		allowed map[string]bool
	}{
		{http.MethodGet, "/account", map[string]bool{accesscontrol.RoleOwner: true, accesscontrol.RoleAdmin: true, accesscontrol.RoleBuilder: true, accesscontrol.RoleViewer: true}},
		{http.MethodGet, "/audit/export", map[string]bool{accesscontrol.RoleOwner: true, accesscontrol.RoleAdmin: true}},
		{http.MethodPut, "/account", map[string]bool{accesscontrol.RoleOwner: true}},
		{http.MethodPost, "/workspace/buckets", map[string]bool{accesscontrol.RoleOwner: true, accesscontrol.RoleAdmin: true}},
		{http.MethodPost, "/integrations/preview_openapi", map[string]bool{accesscontrol.RoleOwner: true, accesscontrol.RoleAdmin: true}},
		// Recovery must require the same mutation grant so every importer can inspect
		// its outcome without widening plan metadata to read-only catalogue roles.
		{http.MethodGet, "/integrations/import/operations/" + uuid.NewString(), map[string]bool{accesscontrol.RoleOwner: true, accesscontrol.RoleAdmin: true}},
		{http.MethodGet, "/credits/pricing", map[string]bool{accesscontrol.RoleOwner: true, accesscontrol.RoleAdmin: true, accesscontrol.RoleBuilder: true, accesscontrol.RoleViewer: true}},
	}
	roles := []string{accesscontrol.RoleOwner, accesscontrol.RoleAdmin, accesscontrol.RoleBuilder, accesscontrol.RoleViewer}
	for _, test := range tests {
		for _, role := range roles {
			t.Run(role+" "+test.method+" "+test.path, func(t *testing.T) {
				called := false
				handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					called = true
					w.WriteHeader(http.StatusNoContent)
				}))
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, requestWithActor(t, test.method, test.path, workspaceRoleActor(t, workspaceID, role)))
				if called != test.allowed[role] {
					t.Fatalf("called/status = %v/%d, allowed=%v", called, response.Code, test.allowed[role])
				}
			})
		}
	}
}

// TestImportStatusRecoveryUsesImportGrantAndSlimDenialContract proves the
// original mutation grant can recover status while read-only actors get safe fields.
func TestImportStatusRecoveryUsesImportGrantAndSlimDenialContract(t *testing.T) {
	workspaceID, operationID := uuid.New(), uuid.New()
	path := "/integrations/import/operations/" + operationID.String()
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	importer := actorWithGrants(t, workspaceID, accesscontrol.Grant{
		Permission: accesscontrol.PermissionCatalogueImport,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
	})
	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, requestWithActor(t, http.MethodGet, path, importer))
	// Import-only custom grants must retain the exact recovery path.
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("import status grant = %d: %s", allowed.Code, allowed.Body.String())
	}
	viewer := workspaceRoleActor(t, workspaceID, accesscontrol.RoleViewer)
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, requestWithActor(t, http.MethodGet, path, viewer))
	var envelope struct {
		Error importControlError `json:"error"`
	}
	// Denials before Registry must still carry the slim import recovery contract.
	if err := json.Unmarshal(denied.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode import denial: %v", err)
	}
	if denied.Code != http.StatusForbidden || envelope.Error.Code != "IMPORT_AUTHORIZATION_DENIED" || envelope.Error.OperationID != operationID.String() || envelope.Error.CommitState != "unknown" || envelope.Error.Recovery != "fused-cli whoami" {
		t.Fatalf("import denial = status %d, envelope %#v", denied.Code, envelope.Error)
	}
}

func TestEveryMultiPermissionRESTPolicyDeniesEachMissingPermissionWithoutSideEffects(t *testing.T) {
	workspaceID := uuid.New()
	dynamicRequirement := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionWorkspaceUpdate,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
	}
	resolver := fixedControlRequirementResolver{requirement: dynamicRequirement}
	for _, policy := range controlRESTPolicies {
		path, params := concretePolicyPath(policy.pattern)
		query := policyQuery(policy)
		requirements, valid := materializeRouteRequirements(policy.requirements, params, query, workspaceID)
		if !valid {
			t.Fatalf("materialize %s %s", policy.method, policy.pattern)
		}
		if dynamicControlRequirements[policy.method+" "+policy.pattern] != "" {
			requirements = append(requirements, dynamicRequirement)
		}
		requirements = uniqueTestRequirements(requirements)
		if len(requirements) < 2 {
			continue
		}
		for removed := range requirements {
			name := policy.method + " " + policy.pattern + " without " + string(requirements[removed].Permission)
			t.Run(name, func(t *testing.T) {
				grants := make([]accesscontrol.Grant, 0, len(requirements)-1)
				for i, requirement := range requirements {
					if i != removed {
						grants = append(grants, accesscontrol.Grant{Permission: requirement.Permission, Resource: requirement.Resource})
					}
				}
				called := false
				handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{}, resolver)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					called = true
				}))
				request := requestWithActor(t, policy.method, path, actorWithGrants(t, workspaceID, grants...))
				request.URL.RawQuery = query.Encode()
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusForbidden || called {
					t.Fatalf("status/called = %d/%v", response.Code, called)
				}
			})
		}
	}
}

type dynamicAuthorizationFixture struct {
	method   string
	path     string
	body     string
	resolver controlRequirementResolver
}

func TestDynamicMultiPermissionPoliciesDenyEachMissingPermissionWithoutSideEffects(t *testing.T) {
	workspaceID := uuid.New()
	fixtures := []struct {
		name  string
		build func(uuid.UUID) dynamicAuthorizationFixture
	}{
		{"workspace apply", workspaceApplyAuthorizationFixture},
		{"desired config apply", desiredConfigApplyAuthorizationFixture},
		{"SDK generate", sdkGenerateAuthorizationFixture},
		{"workspace plan", workspacePlanAuthorizationFixture},
		{"desired config plan", desiredConfigPlanAuthorizationFixture},
		{"plan action", configPlanActionAuthorizationFixture},
	}
	for _, test := range fixtures {
		t.Run(test.name, func(t *testing.T) {
			fixture := test.build(workspaceID)
			requirements := resolveFixtureRequirements(t, workspaceID, fixture)
			if len(requirements) < 2 {
				t.Fatalf("resolved only %d requirement(s): %#v", len(requirements), requirements)
			}
			assertEachMissingRequirementDenies(t, workspaceID, fixture, requirements)
		})
	}
}

func resolveFixtureRequirements(t *testing.T, workspaceID uuid.UUID, fixture dynamicAuthorizationFixture) []accesscontrol.Requirement {
	t.Helper()
	request := dynamicFixtureRequest(t, fixture, actorWithGrants(t, workspaceID))
	requirements, _, ok := resolveControlRESTPolicy(request, fixture.resolver)
	if !ok {
		t.Fatalf("failed to resolve policy for %s %s", fixture.method, fixture.path)
	}
	return uniqueTestRequirements(requirements)
}

func assertEachMissingRequirementDenies(t *testing.T, workspaceID uuid.UUID, fixture dynamicAuthorizationFixture, requirements []accesscontrol.Requirement) {
	t.Helper()
	for removed, missing := range requirements {
		t.Run(string(missing.Permission)+" "+missing.Resource.ID.String(), func(t *testing.T) {
			grants := grantsWithoutRequirement(requirements, removed)
			mutations := 0
			handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{}, fixture.resolver)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				mutations++
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, dynamicFixtureRequest(t, fixture, actorWithGrants(t, workspaceID, grants...)))
			if response.Code != http.StatusForbidden || mutations != 0 {
				t.Fatalf("status/downstream mutations = %d/%d, want 403/0", response.Code, mutations)
			}
		})
	}
}

func grantsWithoutRequirement(requirements []accesscontrol.Requirement, removed int) []accesscontrol.Grant {
	grants := make([]accesscontrol.Grant, 0, len(requirements)-1)
	for index, requirement := range requirements {
		if index == removed {
			continue
		}
		grants = append(grants, accesscontrol.Grant{Permission: requirement.Permission, Resource: requirement.Resource})
	}
	return grants
}

func dynamicFixtureRequest(t *testing.T, fixture dynamicAuthorizationFixture, actor accesscontrol.Actor) *http.Request {
	t.Helper()
	request := httptest.NewRequest(fixture.method, fixture.path, strings.NewReader(fixture.body))
	return request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
}

func workspaceApplyAuthorizationFixture(workspaceID uuid.UUID) dynamicAuthorizationFixture {
	firstServiceID, secondServiceID, bucketID, planID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	stores := &controlRequirementStoreStub{buckets: []store.Bucket{{ID: bucketID, Name: "production"}}}
	plans := &controlConfigRepositoryStub{plan: &store.ConfigPlan{
		ID: planID, ConfigType: store.ConfigTypeWorkspace,
		Actions: []byte(`[{"type":"add_service","service_id":"` + firstServiceID.String() + `"},{"type":"remove_service","service_id":"` + secondServiceID.String() + `"}]`),
	}}
	return dynamicAuthorizationFixture{
		method: http.MethodPost, path: "/workspace/config/apply",
		body:     `{"plan_id":"` + planID.String() + `","auth_materials":{"production\u0000payments":{}}}`,
		resolver: newControlRequirementResolver(stores, plans),
	}
}

func desiredConfigApplyAuthorizationFixture(workspaceID uuid.UUID) dynamicAuthorizationFixture {
	appID, firstServiceID, secondServiceID := uuid.New(), uuid.New(), uuid.New()
	bucketID, planID := uuid.New(), uuid.New()
	stores := &controlRequirementStoreStub{
		buckets: []store.Bucket{{ID: bucketID, Name: "production"}},
		apps: map[uuid.UUID]store.App{
			appID: {AppID: appID, AppFamilyID: appID},
		},
	}
	plans := &controlConfigRepositoryStub{
		plan: &store.ConfigPlan{
			ID: planID, ConfigKey: "sdk:test", ConfigType: store.ConfigTypeSDK, BaseGeneration: 2,
			ResolvedPayload: []byte(`{"bucket_id":"` + bucketID.String() + `","selections":[{"service_id":"` + firstServiceID.String() + `"},{"service_id":"` + secondServiceID.String() + `"}]}`),
			DesiredState:    []byte(`{"bucket":"production"}`),
		},
		state: &store.ConfigState{LatestResourceID: &appID},
	}
	return dynamicAuthorizationFixture{
		method: http.MethodPost, path: "/sdk-config/apply", body: `{"plan_id":"` + planID.String() + `"}`,
		resolver: newControlRequirementResolver(stores, plans),
	}
}

func sdkGenerateAuthorizationFixture(uuid.UUID) dynamicAuthorizationFixture {
	firstServiceID, secondServiceID, bucketID := uuid.New(), uuid.New(), uuid.New()
	body := `{"selections":[{"service_id":"` + firstServiceID.String() + `"},{"service_id":"` + secondServiceID.String() + `"}],"bucket_id":"` + bucketID.String() + `"}`
	return dynamicAuthorizationFixture{
		method: http.MethodPost, path: "/sdks/generate", body: body,
		resolver: newControlRequirementResolver(&controlRequirementStoreStub{}, &controlConfigRepositoryStub{}),
	}
}

func workspacePlanAuthorizationFixture(uuid.UUID) dynamicAuthorizationFixture {
	serviceID, bucketID := uuid.New(), uuid.New()
	stores := &controlRequirementStoreStub{buckets: []store.Bucket{{ID: bucketID, Name: "production"}}}
	body := `{"config":{"services":{"payments":{"service_id":"` + serviceID.String() + `"}},"buckets":{"production":{}}}}`
	return dynamicAuthorizationFixture{
		method: http.MethodPost, path: "/workspace/config/plan", body: body,
		resolver: newControlRequirementResolver(stores, &controlConfigRepositoryStub{}),
	}
}

func desiredConfigPlanAuthorizationFixture(uuid.UUID) dynamicAuthorizationFixture {
	firstServiceID, secondServiceID, bucketID := uuid.New(), uuid.New(), uuid.New()
	stores := &controlRequirementStoreStub{
		services: []store.WorkspaceService{{ServiceID: firstServiceID}, {ServiceID: secondServiceID}},
		buckets:  []store.Bucket{{ID: bucketID, Name: "production"}},
	}
	body := `{"config_key":"sdk:payments:1.0.0","config":{"bucket":"production","services":{"payments":{"version":"1"},"identity":{"version":"1"}}}}`
	return dynamicAuthorizationFixture{
		method: http.MethodPost, path: "/sdk-config/plan", body: body,
		resolver: newControlRequirementResolver(stores, &controlConfigRepositoryStub{}),
	}
}

func configPlanActionAuthorizationFixture(uuid.UUID) dynamicAuthorizationFixture {
	firstServiceID, secondServiceID, planID := uuid.New(), uuid.New(), uuid.New()
	plans := &controlConfigRepositoryStub{plan: &store.ConfigPlan{ID: planID, ConfigType: store.ConfigTypeWorkspace}}
	body := `{"actions":[{"type":"add_service","service_id":"` + firstServiceID.String() + `"},{"type":"remove_service","service_id":"` + secondServiceID.String() + `"}]}`
	return dynamicAuthorizationFixture{
		method: http.MethodPatch, path: "/config/plans/" + planID.String() + "/actions", body: body,
		resolver: newControlRequirementResolver(&controlRequirementStoreStub{}, plans),
	}
}

func uniqueTestRequirements(requirements []accesscontrol.Requirement) []accesscontrol.Requirement {
	seen := make(map[accesscontrol.Requirement]struct{}, len(requirements))
	unique := make([]accesscontrol.Requirement, 0, len(requirements))
	for _, requirement := range requirements {
		if _, duplicate := seen[requirement]; duplicate {
			continue
		}
		seen[requirement] = struct{}{}
		unique = append(unique, requirement)
	}
	return unique
}

func workspaceRoleActor(t *testing.T, workspaceID uuid.UUID, slug string) accesscontrol.Actor {
	t.Helper()
	for _, role := range accesscontrol.BuiltInRoles() {
		if role.Slug != slug {
			continue
		}
		grants := make([]accesscontrol.Grant, 0, len(role.Permissions))
		for _, permission := range role.Permissions {
			grants = append(grants, accesscontrol.Grant{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}})
		}
		return actorWithGrants(t, workspaceID, grants...)
	}
	t.Fatalf("role %q not found", slug)
	return accesscontrol.Actor{}
}

func concretePolicyPath(pattern string) (string, map[string]string) {
	params := make(map[string]string)
	path := regexp.MustCompile(`\{([^}]+)\}`).ReplaceAllStringFunc(pattern, func(token string) string {
		name := strings.Trim(token, "{}")
		value := uuid.NewString()
		params[name] = value
		return value
	})
	return path, params
}

func policyQuery(policy controlRoutePolicy) url.Values {
	query := make(url.Values)
	for _, requirement := range policy.requirements {
		if requirement.source == resourceFromQuery {
			query[requirement.pathParam] = []string{uuid.NewString()}
		}
	}
	return query
}

func actorWithGrants(t *testing.T, workspaceID uuid.UUID, grants ...accesscontrol.Grant) accesscontrol.Actor {
	t.Helper()
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
	if err != nil {
		t.Fatalf("NewAuthorizationSnapshot: %v", err)
	}
	return accesscontrol.Actor{WorkspaceID: workspaceID, SubjectID: uuid.New(), Authorization: snapshot}
}

func ownerActor(t *testing.T, workspaceID uuid.UUID) accesscontrol.Actor {
	t.Helper()
	grants := make([]accesscontrol.Grant, 0, len(accesscontrol.AllPermissions()))
	for _, permission := range accesscontrol.AllPermissions() {
		grants = append(grants, accesscontrol.Grant{
			Permission: permission,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
		})
	}
	return actorWithGrants(t, workspaceID, grants...)
}

func requestWithActor(t *testing.T, method, path string, actor accesscontrol.Actor) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	return request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
}

func assertControlDenial(t *testing.T, response *httptest.ResponseRecorder, status int, code string, missing int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, status, response.Body.String())
	}
	var body struct {
		Error   string            `json:"error"`
		Missing []json.RawMessage `json:"missing"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error != code || len(body.Missing) != missing {
		t.Fatalf("response error/missing = %q/%d, want %q/%d", body.Error, len(body.Missing), code, missing)
	}
}
