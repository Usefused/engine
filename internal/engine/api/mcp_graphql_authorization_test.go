package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

type appAccessResolverTestStore struct {
	*workspaceTestStore
	families map[uuid.UUID]uuid.UUID
	calls    int
}

func (s *appAccessResolverTestStore) ResolveAppFamilyAccess(_ context.Context, _ uuid.UUID, appIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	s.calls++
	resolved := make(map[uuid.UUID]uuid.UUID, len(appIDs))
	for _, appID := range appIDs {
		if familyID := s.families[appID]; familyID != uuid.Nil {
			resolved[appID] = familyID
		}
	}
	return resolved, nil
}

func TestGraphQLAppRequirementsResolveFamiliesInOneCall(t *testing.T) {
	appOne, appTwo, familyID := uuid.New(), uuid.New(), uuid.New()
	s := &appAccessResolverTestStore{
		workspaceTestStore: &workspaceTestStore{},
		families:           map[uuid.UUID]uuid.UUID{appOne: familyID, appTwo: familyID},
	}
	requests := []graphQLAppRequirement{
		{appID: appOne, permission: accesscontrol.PermissionAppRead},
		{appID: appTwo, permission: accesscontrol.PermissionAppRead},
	}
	requirements, err := (graphQLAuthorizationResources{store: s}).resolveApps(t.Context(), uuid.New(), requests)
	want := accesscontrol.Requirement{Permission: accesscontrol.PermissionAppRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID}}
	if err != nil || s.calls != 1 || len(requirements) != 1 || requirements[0] != want {
		t.Fatalf("requirements/calls/error = %#v/%d/%v, want %#v/1/nil", requirements, s.calls, err, want)
	}
}

// TestMCPAnalyticsAuthorizationResolvesAppIDToFamily keeps both execution
// permissions bound to the exact app family selected by the immutable app ID.
func TestMCPAnalyticsAuthorizationResolvesAppIDToFamily(t *testing.T) {
	appID, familyID, workspaceID := uuid.New(), uuid.New(), uuid.New()
	baseStore := &workspaceTestStore{}
	schema := authorizationTestSchema(t, baseStore)
	body, err := json.Marshal(map[string]any{
		"query": `query { mcpAnalytics(app_id: "` + appID.String() + `") { total_requests } }`,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	plan, err := buildGraphQLAuthorizationPlan(&schema, body, workspaceID)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if len(plan.apps) != 2 || plan.apps[0].appID != appID || plan.apps[1].appID != appID {
		t.Fatalf("app requirements = %#v, want exact app id %s", plan.apps, appID)
	}

	resolverStore := &appAccessResolverTestStore{
		workspaceTestStore: baseStore,
		families:           map[uuid.UUID]uuid.UUID{appID: familyID},
	}
	requirements, err := (graphQLAuthorizationResources{store: resolverStore}).resolveApps(t.Context(), workspaceID, plan.apps)
	want := map[accesscontrol.Permission]bool{
		accesscontrol.PermissionAppRead:   true,
		accesscontrol.PermissionAuditRead: true,
	}
	if err != nil || resolverStore.calls != 1 || len(requirements) != len(want) {
		t.Fatalf("requirements/calls/error = %#v/%d/%v, want two requirements/1/nil", requirements, resolverStore.calls, err)
	}
	for _, requirement := range requirements {
		if !want[requirement.Permission] || requirement.Resource != (accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID}) {
			t.Fatalf("unexpected resolved requirement: %#v", requirement)
		}
	}
}

func TestEngineGraphQLPolicyClassifiesEveryRootResolver(t *testing.T) {
	schema := authorizationTestSchema(t, &workspaceTestStore{})
	assertRootPolicyCoverage(t, schema.QueryType(), engineGraphQLPolicy.queryRoots)
	assertRootPolicyCoverage(t, schema.MutationType(), engineGraphQLPolicy.mutationRoots)
}

func TestWorkspaceExecutionAnalyticsRequiresAuditRead(t *testing.T) {
	workspaceID := uuid.New()
	s := &workspaceTestStore{accountID: uuid.New()}
	schema := authorizationTestSchema(t, s)
	handler := mcpGraphQLHandler(schema)
	actor := actorWithWorkspacePermissions(t, workspaceID, accesscontrol.PermissionWorkspaceRead)
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"query { workspaceExecutionAnalytics { total_calls } }"}`))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
	}
	if s.workspaceExecutionCalls != 0 {
		t.Fatalf("resolver calls = %d, want 0", s.workspaceExecutionCalls)
	}
}

// TestWorkspaceNotificationsRequiresWorkspaceRead keeps notification discovery
// separate from execution-audit visibility.
func TestWorkspaceNotificationsRequiresWorkspaceRead(t *testing.T) {
	workspaceID := uuid.New()
	schema := authorizationTestSchema(t, &workspaceTestStore{})
	handler := mcpGraphQLHandler(schema)
	actor := actorWithWorkspacePermissions(t, workspaceID, accesscontrol.PermissionAuditRead)
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"query { workspaceNotifications { total_count } }"}`))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
	}
}

// TestMCPAnalyticsRequiresAppAndAuditRead proves app visibility alone cannot
// expose MCP execution history and that the complete capability set succeeds.
func TestMCPAnalyticsRequiresAppAndAuditRead(t *testing.T) {
	appID, familyID, workspaceID := uuid.New(), uuid.New(), uuid.New()
	baseStore := &workspaceTestStore{}
	schema := authorizationTestSchema(t, baseStore)
	resolverStore := &appAccessResolverTestStore{
		workspaceTestStore: baseStore,
		families:           map[uuid.UUID]uuid.UUID{appID: familyID},
	}
	handler := mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: resolverStore})
	body := `{"query":"query { mcpAnalytics(app_id: \"` + appID.String() + `\") { total_requests } }"}`
	appOnly := actorWithResourcePermissions(t, workspaceID, accesscontrol.Grant{
		Permission: accesscontrol.PermissionAppRead,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID},
	})
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(body))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), appOnly))
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("app-only status = %d, want 403: %s", response.Code, response.Body.String())
	}
	complete := actorWithResourcePermissions(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionAuditRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
	)
	request = httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(body))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), complete))
	response = httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("complete status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

func TestEngineGraphQLRejectsRequirementsBeyondAuditBound(t *testing.T) {
	workspaceID := uuid.New()
	schema := authorizationTestSchema(t, &workspaceTestStore{})
	handler := mcpGraphQLHandler(schema)
	actor := actorWithWorkspacePermissions(t, workspaceID, accesscontrol.PermissionBucketRead)
	var query strings.Builder
	query.WriteString(`{"query":"query {`)
	for range accesscontrol.MaxAuditMissingRequirements + 1 {
		id := uuid.NewString()
		query.WriteString(" x" + strings.ReplaceAll(id, "-", "") + `: bucketSummary(bucket_id: \"` + id + `\") { id }`)
	}
	query.WriteString(` }"}`)
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(query.String()))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
}

func TestEngineGraphQLPolicyValidationRejectsUnclassifiedRoot(t *testing.T) {
	schema := authorizationTestSchema(t, &workspaceTestStore{})
	queryPolicies := make(map[string]graphQLFieldPolicy, len(engineGraphQLPolicy.queryRoots)-1)
	for fieldName, policy := range engineGraphQLPolicy.queryRoots {
		if fieldName != "mcpServers" {
			queryPolicies[fieldName] = policy
		}
	}
	policy := engineGraphQLPolicy
	policy.queryRoots = queryPolicies
	if err := validateGraphQLAuthorizationPolicy(&schema, policy); !errors.Is(err, errGraphQLPolicyMissing) {
		t.Fatalf("error = %v, want errGraphQLPolicyMissing", err)
	}
}

func assertRootPolicyCoverage(t *testing.T, root *graphql.Object, policies map[string]graphQLFieldPolicy) {
	t.Helper()
	for fieldName := range root.Fields() {
		if _, ok := policies[fieldName]; !ok {
			t.Errorf("%s.%s has no authorization policy", root.Name(), fieldName)
		}
	}
	for fieldName := range policies {
		if _, ok := root.Fields()[fieldName]; !ok {
			t.Errorf("authorization policy references missing %s.%s", root.Name(), fieldName)
		}
	}
}

func TestBuildGraphQLAuthorizationPlanHandlesOperationAliasesAndFragments(t *testing.T) {
	workspaceID := uuid.New()
	schema := authorizationTestSchema(t, &workspaceTestStore{})
	body := []byte(`{
		"query":"query Selected { servers: mcpServers { items { ...TokenFields } } ... on EngineQuery { bucketSummary(bucket_id: \"11111111-1111-1111-1111-111111111111\") { id } } } query Ignored { workspaceServices { id } } fragment TokenFields on MCPServer { execution_token }",
		"operationName":"Selected"
	}`)

	plan, err := buildGraphQLAuthorizationPlan(&schema, body, workspaceID)
	if err != nil {
		t.Fatalf("buildGraphQLAuthorizationPlan() error = %v", err)
	}
	want := map[accesscontrol.Permission]bool{
		accesscontrol.PermissionAppTokensManage: true,
		accesscontrol.PermissionBucketRead:      true,
	}
	if len(plan.requirements) != len(want) || plan.rootFields != 2 {
		t.Fatalf("plan = %#v, want 2 direct requirements across 2 roots", plan)
	}
	for _, requirement := range plan.requirements {
		if !want[requirement.Permission] {
			t.Errorf("unexpected requirement: %#v", requirement)
		}
		if requirement.Permission == accesscontrol.PermissionBucketRead && requirement.Resource.Type != accesscontrol.ResourceBucket {
			t.Errorf("bucket requirement = %#v, want bucket scope", requirement)
		}
		if requirement.Permission != accesscontrol.PermissionBucketRead && requirement.Resource.ID != workspaceID {
			t.Errorf("workspace requirement = %#v, want workspace %s", requirement, workspaceID)
		}
	}
	if len(plan.scopes) != 1 || plan.scopes[0].permission != accesscontrol.PermissionAppRead || plan.scopes[0].resource != accesscontrol.ResourceApp {
		t.Fatalf("collection scopes = %#v, want app.read", plan.scopes)
	}
}

func TestEngineGraphQLPreflightIsAllOrNothing(t *testing.T) {
	workspaceID := uuid.New()
	s := &workspaceTestStore{accountID: uuid.New(), mockScopes: map[uuid.UUID]*store.AppRuntime{}}
	schema := authorizationTestSchema(t, s)
	handler := mcpGraphQLHandler(schema)
	actor := actorWithWorkspacePermissions(t, workspaceID, accesscontrol.PermissionAppRead)
	body := `{"query":"query { first: mcpServers { total } second: bucketSummaries { id } }"}`
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
	}
	if s.listMCPScopesCalls != 0 {
		t.Fatalf("resolver ran %d time(s) before operation-wide authorization completed", s.listMCPScopesCalls)
	}
	if !strings.Contains(response.Header().Get("Server-Timing"), "engine_authz;dur=") {
		t.Fatalf("Server-Timing = %q, want engine_authz", response.Header().Get("Server-Timing"))
	}
}

func TestEngineGraphQLProtectedChildAddsPermission(t *testing.T) {
	workspaceID := uuid.New()
	s := &workspaceTestStore{accountID: uuid.New()}
	schema := authorizationTestSchema(t, s)
	handler := mcpGraphQLHandler(schema)
	actor := actorWithWorkspacePermissions(t, workspaceID, accesscontrol.PermissionAppRead)
	body := `{"query":"query { mcpServers { items { ... on MCPServer { execution_token } } } }"}`
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(body))
	captureContext, capture := accesscontrol.ContextWithRequiredPermissionsCapture(request.Context())
	request = request.WithContext(accesscontrol.ContextWithActor(captureContext, actor))
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
	}
	var denial struct {
		Missing []struct {
			Permission accesscontrol.Permission `json:"permission"`
		} `json:"missing"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &denial); err != nil {
		t.Fatalf("decode denial: %v", err)
	}
	if len(denial.Missing) != 1 || denial.Missing[0].Permission != accesscontrol.PermissionAppTokensManage {
		t.Fatalf("missing = %#v, want app.tokens.manage", denial.Missing)
	}
	captured, ok := capture.RequiredPermissions()
	if !ok || !hasRequirementPermission(captured, accesscontrol.PermissionAppTokensManage) {
		t.Fatalf("captured authorization requirements = %#v, %v", captured, ok)
	}
}

func TestEngineGraphQLAppTokensRequiresManagePermissionForRequestedFamily(t *testing.T) {
	workspaceID := uuid.New()
	familyID := uuid.New()
	s := &workspaceTestStore{accountID: uuid.New()}
	schema := authorizationTestSchema(t, s)
	handler := mcpGraphQLHandler(schema)
	actor := actorWithWorkspacePermissions(t, workspaceID, accesscontrol.PermissionAppRead)
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"query { appTokens(app_family_id: \"`+familyID.String()+`\") { id allow expires_at } }"}`))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
	}
	var denial struct {
		Missing []struct {
			Permission accesscontrol.Permission `json:"permission"`
			ResourceID uuid.UUID                `json:"resource_id"`
		} `json:"missing"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &denial); err != nil {
		t.Fatalf("decode denial: %v", err)
	}
	if len(denial.Missing) != 1 || denial.Missing[0].Permission != accesscontrol.PermissionAppTokensManage || denial.Missing[0].ResourceID != familyID {
		t.Fatalf("missing = %#v, want app.tokens.manage for %s", denial.Missing, familyID)
	}
}

func hasRequirementPermission(requirements []accesscontrol.Requirement, permission accesscontrol.Permission) bool {
	for _, requirement := range requirements {
		if requirement.Permission == permission {
			return true
		}
	}
	return false
}

func TestEngineGraphQLProtectedCollectionRejectsDisjointScopes(t *testing.T) {
	workspaceID, readableArtifact, tokenArtifact := uuid.New(), uuid.New(), uuid.New()
	s := &workspaceTestStore{accountID: uuid.New(), mockScopes: map[uuid.UUID]*store.AppRuntime{
		readableArtifact: {AccountID: uuid.New(), AppID: readableArtifact, Kind: "mcp"},
	}}
	schema := authorizationTestSchema(t, s)
	handler := mcpGraphQLHandler(schema)
	actor := actorWithResourcePermissions(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: readableArtifact}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppTokensManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: tokenArtifact}},
	)
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"query { mcpServers { items { id execution_token } } }"}`))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want disjoint scopes denied: %s", response.Code, response.Body.String())
	}
	if s.listMCPScopesCalls != 0 {
		t.Fatalf("resolver ran before protected collection authorization: calls=%d", s.listMCPScopesCalls)
	}
}

func TestEngineGraphQLCollectionsFilterAndCountAuthorizedResources(t *testing.T) {
	workspaceID := uuid.New()
	accountID := uuid.New()
	allowedArtifact, deniedArtifact := uuid.New(), uuid.New()
	allowedBucket, deniedBucket := uuid.New(), uuid.New()
	allowedService, deniedService := uuid.New(), uuid.New()
	now := time.Now().UTC()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			allowedArtifact: {AccountID: accountID, AppID: allowedArtifact, Kind: "mcp", Name: "allowed", CreatedAt: now},
			deniedArtifact:  {AccountID: accountID, AppID: deniedArtifact, Kind: "mcp", Name: "denied", CreatedAt: now.Add(-time.Minute)},
		},
		bucketSummaries: []store.BucketSummary{{Bucket: store.Bucket{ID: allowedBucket, Name: "allowed"}}, {Bucket: store.Bucket{ID: deniedBucket, Name: "denied"}}},
		workspaceServices: []store.WorkspaceService{
			{ServiceID: allowedService, ServiceName: "Allowed", CreatedAt: now},
			{ServiceID: deniedService, ServiceName: "Denied", CreatedAt: now.Add(-time.Minute)},
		},
	}
	schema := authorizationTestSchema(t, s)
	handler := mcpGraphQLHandler(schema)
	actor := actorWithResourcePermissions(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: allowedArtifact}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionBucketRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: allowedBucket}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: allowedService}},
	)
	actor.AccountID = accountID
	query := `query {
		mcpServers(limit: 10, offset: 0) { total items { id } }
		bucketSummaryPage(limit: 10, offset: 0) { total items { id } }
		workspaceServicePage(limit: 10, offset: 0) { total data { service_id } }
	}`
	body, _ := json.Marshal(map[string]any{"query": query})
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Data map[string]map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for field, result := range payload.Data {
		if result["total"] != float64(1) {
			t.Errorf("%s total = %#v, want scoped total 1", field, result["total"])
		}
	}
}

func TestEngineGraphQLRelatedCollectionsIntersectScopesBeforeTotals(t *testing.T) {
	workspaceID, accountID := uuid.New(), uuid.New()
	allowedArtifact, deniedArtifact := uuid.New(), uuid.New()
	allowedBucket, deniedBucket := uuid.New(), uuid.New()
	allowedService, deniedService := uuid.New(), uuid.New()
	now := time.Now().UTC()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			allowedArtifact: {AccountID: accountID, AppID: allowedArtifact, Kind: "sdk", Name: "allowed", CreatedAt: now},
		},
		sdkBuckets: map[uuid.UUID][]store.Bucket{allowedArtifact: {
			{ID: allowedBucket, Name: "allowed"}, {ID: deniedBucket, Name: "denied"},
		}},
		appRuntimesForBucket: map[uuid.UUID][]store.AppRuntime{allowedBucket: {
			{AccountID: accountID, AppID: allowedArtifact, Kind: "sdk", Name: "allowed", CreatedAt: now},
			{AccountID: accountID, AppID: deniedArtifact, Kind: "sdk", Name: "denied", CreatedAt: now.Add(-time.Minute)},
		}},
		bucketServiceSummaries: map[uuid.UUID][]store.BucketServiceSummary{allowedBucket: {
			{ServiceID: allowedService, ServiceName: "Allowed"}, {ServiceID: deniedService, ServiceName: "Denied"},
		}},
	}
	schema := authorizationTestSchema(t, s)
	handler := mcpGraphQLHandler(schema)
	actor := actorWithResourcePermissions(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: allowedArtifact}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionBucketRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: allowedBucket}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: allowedService}},
	)
	actor.AccountID = accountID
	query := `query {
		sdkBuckets(app_family_id: "` + allowedArtifact.String() + `") { id }
		bucketSDKPage(bucket_id: "` + allowedBucket.String() + `", limit: 10, offset: 0) { total items { id } }
		bucketServicePage(bucket_id: "` + allowedBucket.String() + `", limit: 10, offset: 0) { total items { service_id } }
	}`
	body, _ := json.Marshal(map[string]any{"query": query})
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	data := payload["data"].(map[string]any)
	if graphqlErrors, ok := payload["errors"]; ok {
		t.Fatalf("GraphQL errors: %#v; body=%s", graphqlErrors, response.Body.String())
	}
	buckets, ok := data["sdkBuckets"].([]any)
	if !ok || len(buckets) != 1 || buckets[0].(map[string]any)["id"] != allowedBucket.String() {
		t.Fatalf("sdkBuckets = %#v, want only allowed bucket", buckets)
	}
	for _, field := range []string{"bucketSDKPage", "bucketServicePage"} {
		page := data[field].(map[string]any)
		if page["total"] != float64(1) || len(page["items"].([]any)) != 1 {
			t.Fatalf("%s = %#v, want scoped total/items 1", field, page)
		}
	}
}

func TestBuildGraphQLAuthorizationPlanUsesVariableResourceScope(t *testing.T) {
	workspaceID, bucketID := uuid.New(), uuid.New()
	schema := authorizationTestSchema(t, &workspaceTestStore{})
	body, err := json.Marshal(map[string]any{
		"query":     `query Bucket($id: String!) { bucketSummary(bucket_id: $id) { __typename id } }`,
		"variables": map[string]any{"id": bucketID.String()},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	plan, err := buildGraphQLAuthorizationPlan(&schema, body, workspaceID)
	if err != nil {
		t.Fatalf("buildGraphQLAuthorizationPlan() error = %v", err)
	}
	want := accesscontrol.Requirement{
		Permission: accesscontrol.PermissionBucketRead,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID},
	}
	if len(plan.requirements) != 1 || plan.requirements[0] != want {
		t.Fatalf("requirements = %#v, want %#v", plan.requirements, want)
	}
	actor := actorWithResourcePermissions(t, workspaceID, accesscontrol.Grant(want))
	if err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(t.Context(), actor, plan.requirements...); err != nil {
		t.Fatalf("bucket-scoped grant should authorize exact argument: %v", err)
	}
}

func TestBuildGraphQLAuthorizationPlanProtectsOwnerRoleValues(t *testing.T) {
	workspaceID, teamID := uuid.New(), uuid.New()
	schema := authorizationTestSchema(t, &workspaceTestStore{})
	tests := []struct {
		name        string
		query       string
		variables   map[string]any
		wantAccount bool
	}{
		{name: "literal Owner", query: `mutation { setTeamWorkspaceRole(team_id:"` + teamID.String() + `",role:OWNER) { changed } }`, wantAccount: true},
		{name: "variable Owner", query: `mutation SetRole($team:ID!,$role:TeamWorkspaceRole) { setTeamWorkspaceRole(team_id:$team,role:$role) { changed } }`, variables: map[string]any{"team": teamID.String(), "role": "OWNER"}, wantAccount: true},
		{name: "ordinary Admin", query: `mutation { setTeamWorkspaceRole(team_id:"` + teamID.String() + `",role:ADMIN) { changed } }`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"query": test.query, "variables": test.variables})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			plan, err := buildGraphQLAuthorizationPlan(&schema, body, workspaceID)
			if err != nil {
				t.Fatalf("build plan: %v", err)
			}
			if !hasRequirementPermission(plan.requirements, accesscontrol.PermissionAccessManage) {
				t.Fatalf("requirements = %#v, want access.manage", plan.requirements)
			}
			if got := hasRequirementPermission(plan.requirements, accesscontrol.PermissionAccountManage); got != test.wantAccount {
				t.Fatalf("account.manage requirement = %v, want %v: %#v", got, test.wantAccount, plan.requirements)
			}
		})
	}
}

func TestDeployMCPAuthorizationResolvesBucketAndEveryServiceInBatches(t *testing.T) {
	workspaceID, bucketID := uuid.New(), uuid.New()
	firstService, secondService := uuid.New(), uuid.New()
	s := &workspaceTestStore{buckets: []store.Bucket{{ID: bucketID, Name: "production"}}}
	resolver := &mockVerifier{slugIDs: map[string]uuid.UUID{"github": firstService, "linear": secondService}}
	schema := authorizationTestSchema(t, s)
	body, err := json.Marshal(map[string]any{
		"query": `mutation Deploy($config: EngineJSON!) { deployMcpServer(config: $config) { id } }`,
		"variables": map[string]any{"config": map[string]any{
			"apiVersion": "fused/v1", "kind": "mcp", "name": "team-agent", "version": "1.0.0", "bucket": "production",
			"services": map[string]any{"github": map[string]any{"version": "1.0.0"}, "linear": map[string]any{"version": "1.0.0"}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	plan, err := buildGraphQLAuthorizationPlan(&schema, body, workspaceID)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	configStore := &mockConfigStore{}
	resources := graphQLAuthorizationResources{store: s, configStore: configStore, slugResolver: resolver}
	dynamic, err := resources.resolveDeployments(t.Context(), workspaceID, plan.deployments, "test-key")
	if err != nil {
		t.Fatalf("resolve deployment requirements: %v", err)
	}
	plan.mergeRequirements(dynamic)
	if resolver.resolveCalls != 1 || len(resolver.resolvedSlugs) != 2 {
		t.Fatalf("service resolution calls/slugs = %d/%v, want one batch", resolver.resolveCalls, resolver.resolvedSlugs)
	}
	actor := actorWithResourcePermissions(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionWorkspaceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: firstService}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: secondService}},
	)
	if err := authorizeGraphQLPlan(t.Context(), actor, plan); err != nil {
		t.Fatalf("complete scoped deployment should authorize: %v", err)
	}
	existingConfigResourceID := uuid.New()
	configStore.states = []store.ConfigState{{
		ConfigKey: "mcp:team-agent:1.0.0", ConfigType: store.ConfigTypeMCP, LatestResourceID: &existingConfigResourceID,
	}}
	updateRequirements, err := resources.resolveDeployments(t.Context(), workspaceID, plan.deployments, "test-key")
	if err != nil {
		t.Fatalf("resolve update requirements: %v", err)
	}
	updatePlan := graphQLAuthorizationPlan{requirements: append([]accesscontrol.Requirement{{
		Permission: accesscontrol.PermissionWorkspaceRead,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
	}}, updateRequirements...)}
	manager := actorWithResourcePermissions(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionWorkspaceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: existingConfigResourceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: firstService}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: secondService}},
	)
	if err := authorizeGraphQLPlan(t.Context(), manager, updatePlan); err != nil {
		t.Fatalf("artifact-scoped Manager should authorize update: %v", err)
	}
	actor = actorWithResourcePermissions(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionWorkspaceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: firstService}},
	)
	if err := authorizeGraphQLPlan(t.Context(), actor, plan); !errors.Is(err, accesscontrol.ErrPermissionDenied) {
		t.Fatalf("missing one selected service error = %v, want permission denied", err)
	}
}

func TestDeployMCPAuthorizationChecksStaticPermissionsBeforeResolution(t *testing.T) {
	workspaceID := uuid.New()
	s := &workspaceTestStore{buckets: []store.Bucket{{ID: uuid.New(), Name: "production"}}}
	resolver := &mockVerifier{slugIDs: map[string]uuid.UUID{"github": uuid.New()}}
	schema := authorizationTestSchema(t, s)
	handler := mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: s, configStore: &mockConfigStore{}, slugResolver: resolver})
	actor := actorWithResourcePermissions(t, workspaceID)
	body, _ := json.Marshal(map[string]any{
		"query": `mutation Deploy($config: EngineJSON!) { deployMcpServer(config: $config) { id } }`,
		"variables": map[string]any{"config": map[string]any{
			"apiVersion": "fused/v1", "kind": "mcp", "name": "team-agent", "version": "1.0.0", "bucket": "production",
			"services": map[string]any{"github": map[string]any{"version": "1.0.0"}},
		}},
	})
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()

	handler(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want static denial: %s", response.Code, response.Body.String())
	}
	if resolver.resolveCalls != 0 {
		t.Fatalf("Registry resolver calls = %d, want zero before static authorization", resolver.resolveCalls)
	}
}

func TestEngineGraphQLConnectionAuthorizationUsesExactBucketAndOneLookup(t *testing.T) {
	workspaceID, allowedBucket, deniedBucket := uuid.New(), uuid.New(), uuid.New()
	allowedConnection, deniedConnection := uuid.New(), uuid.New()
	resourceID := uuid.New()
	s := &workspaceTestStore{
		authConnections: []store.AuthConnection{
			{ID: allowedConnection, BucketID: allowedBucket},
			{ID: deniedConnection, BucketID: deniedBucket},
		},
		connectionResources: map[uuid.UUID][]store.ConnectionResource{
			allowedConnection: {{ID: resourceID, ConnectionID: allowedConnection, BucketID: allowedBucket, DisplayName: "Allowed"}},
		},
	}
	schema := authorizationTestSchema(t, s)
	handler := mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: s})
	actor := actorWithResourcePermissions(t, workspaceID, accesscontrol.Grant{
		Permission: accesscontrol.PermissionConnectionRead,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: allowedBucket},
	})

	query := `query {
		first: connectionResources(connection_id: "` + allowedConnection.String() + `") { id }
		second: connectionResources(connection_id: "` + allowedConnection.String() + `") { display_name }
	}`
	response := doAuthorizedGraphQLRequest(t, handler, actor, query)
	if response.Code != http.StatusOK {
		t.Fatalf("allowed status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if s.getAuthConnectionsByIDsCalls != 1 {
		t.Fatalf("connection batch lookups = %d, want one for duplicate aliases", s.getAuthConnectionsByIDsCalls)
	}

	deniedQuery := `query { connectionResources(connection_id: "` + deniedConnection.String() + `") { id } }`
	response = doAuthorizedGraphQLRequest(t, handler, actor, deniedQuery)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-bucket status = %d, want 403: %s", response.Code, response.Body.String())
	}
	if s.getAuthConnectionsByIDsCalls != 2 {
		t.Fatalf("connection batch lookups = %d, want one per authorized preflight", s.getAuthConnectionsByIDsCalls)
	}
}

func TestBuildGraphQLAuthorizationPlanClassifiesConnectionOperations(t *testing.T) {
	connectionID, workspaceID := uuid.New(), uuid.New()
	schema := authorizationTestSchema(t, &workspaceTestStore{})
	tests := []struct {
		name       string
		query      string
		permission accesscontrol.Permission
	}{
		{name: "list", query: `query { connectionResources(connection_id: "` + connectionID.String() + `") { id } }`, permission: accesscontrol.PermissionConnectionRead},
		{name: "set default", query: `mutation { setDefaultConnectionResource(connection_id: "` + connectionID.String() + `", resource_id: "` + uuid.NewString() + `") { id } }`, permission: accesscontrol.PermissionConnectionManage},
		{name: "rediscover", query: `mutation { rediscoverConnectionResources(connection_id: "` + connectionID.String() + `") { id } }`, permission: accesscontrol.PermissionConnectionManage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"query": test.query})
			plan, err := buildGraphQLAuthorizationPlan(&schema, body, workspaceID)
			if err != nil {
				t.Fatalf("build plan: %v", err)
			}
			if len(plan.connections) != 1 || plan.connections[0].connectionID != connectionID || plan.connections[0].permission != test.permission {
				t.Fatalf("connection requirements = %#v", plan.connections)
			}
			if len(plan.requirements) != 0 {
				t.Fatalf("static requirements = %#v, want no workspace fallback", plan.requirements)
			}
			if len(plan.scopes) != 1 || plan.scopes[0].resource != accesscontrol.ResourceBucket || plan.scopes[0].permission != test.permission {
				t.Fatalf("static scope gate = %#v", plan.scopes)
			}
		})
	}
}

func TestEngineGraphQLConnectionAuthorizationRejectsBeforeLookupWithoutScope(t *testing.T) {
	workspaceID := uuid.New()
	s := &workspaceTestStore{authConnections: []store.AuthConnection{{ID: uuid.New(), BucketID: uuid.New()}}}
	schema := authorizationTestSchema(t, s)
	handler := mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: s})
	actor := actorWithResourcePermissions(t, workspaceID)
	query := `query { connectionResources(connection_id: "` + s.authConnections[0].ID.String() + `") { id } }`

	response := doAuthorizedGraphQLRequest(t, handler, actor, query)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
	}
	if s.getAuthConnectionsByIDsCalls != 0 {
		t.Fatalf("connection lookup ran %d time(s) before static authorization", s.getAuthConnectionsByIDsCalls)
	}
}

func TestEngineGraphQLConnectionManagerCanMutateOwnBucket(t *testing.T) {
	workspaceID, bucketID := uuid.New(), uuid.New()
	connectionID, resourceID := uuid.New(), uuid.New()
	s := &workspaceTestStore{
		authConnections: []store.AuthConnection{{ID: connectionID, BucketID: bucketID}},
		connectionResources: map[uuid.UUID][]store.ConnectionResource{
			connectionID: {{ID: resourceID, ConnectionID: connectionID, BucketID: bucketID}},
		},
	}
	schema := authorizationTestSchema(t, s)
	handler := mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: s})
	actor := actorWithResourcePermissions(t, workspaceID, accesscontrol.Grant{
		Permission: accesscontrol.PermissionConnectionManage,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID},
	})
	mutation := `mutation { setDefaultConnectionResource(connection_id: "` + connectionID.String() + `", resource_id: "` + resourceID.String() + `") { id } }`

	response := doAuthorizedGraphQLRequest(t, handler, actor, mutation)

	if response.Code != http.StatusOK || s.defaultConnectionResourceID != resourceID {
		t.Fatalf("manager mutation status/body/selected = %d/%s/%s", response.Code, response.Body.String(), s.defaultConnectionResourceID)
	}
	if s.getAuthConnectionsByIDsCalls != 1 {
		t.Fatalf("connection batch lookups = %d, want one", s.getAuthConnectionsByIDsCalls)
	}
}

func doAuthorizedGraphQLRequest(t *testing.T, handler http.Handler, actor accesscontrol.Actor, query string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		t.Fatalf("marshal GraphQL request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestBuildGraphQLAuthorizationPlanIntrospectionPolicy(t *testing.T) {
	schema := authorizationTestSchema(t, &workspaceTestStore{})
	body := []byte(`{"query":"query { __schema { queryType { name } } }"}`)
	if _, err := buildGraphQLAuthorizationPlan(&schema, body, uuid.New()); !errors.Is(err, errInvalidGraphQLRequest) {
		t.Fatalf("production introspection error = %v, want fail closed", err)
	}
	plan, err := buildGraphQLAuthorizationPlanWithOptions(&schema, body, uuid.New(), true)
	if err != nil {
		t.Fatalf("development introspection error = %v", err)
	}
	if len(plan.requirements) != 0 {
		t.Fatalf("development introspection requirements = %#v, want authenticated-only", plan.requirements)
	}
}

func TestEngineGraphQLHandlerIntrospectionFollowsEnvironment(t *testing.T) {
	for _, test := range []struct {
		name        string
		environment string
		wantStatus  int
	}{
		{name: "production denied", environment: "production", wantStatus: http.StatusBadRequest},
		{name: "development allowed", environment: "development", wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("FUSED_ENV", test.environment)
			workspaceID := uuid.New()
			s := &workspaceTestStore{accountID: uuid.New()}
			schema := authorizationTestSchema(t, s)
			handler := mcpGraphQLHandler(schema)
			actor := actorWithWorkspacePermissions(t, workspaceID)
			request := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"query { __schema { queryType { name } } }"}`))
			request.Header.Set("Content-Type", "application/json")
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
			response := httptest.NewRecorder()

			handler(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func TestBuildGraphQLAuthorizationPlanFailsClosed(t *testing.T) {
	schema := authorizationTestSchema(t, &workspaceTestStore{})
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"query":"query { futureRoot { id } }"}`},
		{name: "multiple operations without name", body: `{"query":"query A { mcpServers { total } } query B { bucketSummaries { id } }"}`},
		{name: "fragment cycle", body: `{"query":"query { mcpServers { items { ...Loop } } } fragment Loop on MCPServer { ...Loop }"}`},
		{name: "subscription", body: `{"query":"subscription { mcpServers { total } }"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildGraphQLAuthorizationPlan(&schema, []byte(test.body), uuid.New())
			if !errors.Is(err, errInvalidGraphQLRequest) {
				t.Fatalf("error = %v, want errInvalidGraphQLRequest", err)
			}
		})
	}
}

func authorizationTestSchema(t *testing.T, s *workspaceTestStore) graphql.Schema {
	t.Helper()
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, s, &mockVerifier{}, &mockRegistryClient{}, []byte("12345678901234567890123456789012"))
	if err != nil {
		t.Fatalf("newMCPGraphQLSchema() error = %v", err)
	}
	return schema
}

func actorWithWorkspacePermissions(t *testing.T, workspaceID uuid.UUID, values ...accesscontrol.Permission) accesscontrol.Actor {
	t.Helper()
	grants := make([]accesscontrol.Grant, 0, len(values))
	for _, permission := range values {
		grants = append(grants, accesscontrol.Grant{
			Permission: permission,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
		})
	}
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
	if err != nil {
		t.Fatalf("NewAuthorizationSnapshot() error = %v", err)
	}
	return accesscontrol.Actor{AccountID: uuid.New(), WorkspaceID: workspaceID, SubjectID: uuid.New(), Authorization: snapshot}
}

func actorWithResourcePermissions(t *testing.T, workspaceID uuid.UUID, grants ...accesscontrol.Grant) accesscontrol.Actor {
	t.Helper()
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
	if err != nil {
		t.Fatalf("NewAuthorizationSnapshot() error = %v", err)
	}
	return accesscontrol.Actor{AccountID: uuid.New(), WorkspaceID: workspaceID, SubjectID: uuid.New(), Authorization: snapshot}
}
