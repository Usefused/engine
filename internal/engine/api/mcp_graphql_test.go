package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

type artifactReferenceGraphQLTestStore struct {
	*workspaceTestStore
	references   map[string]uuid.UUID
	services     []store.AppServiceSummary
	lastQuery    store.ResourceReferenceQuery
	referenceErr error
}

func (s *artifactReferenceGraphQLTestStore) ListAuthorizedAppsByAccount(_ context.Context, accountID uuid.UUID, _ accesscontrol.AuthorizedScope, kind, _, _ string, _, _ int) ([]store.AppCatalogItem, int, error) {
	items := make([]store.AppCatalogItem, 0)
	for _, scope := range s.mockScopes {
		if scope.AccountID == accountID && (kind == "" || scope.Kind == store.AppKind(kind)) {
			items = append(items, appCatalogItemFromTestScope(scope))
		}
	}
	return items, len(items), nil
}

func (s *artifactReferenceGraphQLTestStore) GetAuthorizedApp(_ context.Context, accountID, appID uuid.UUID, _ accesscontrol.AuthorizedScope) (*store.AppCatalogItem, error) {
	scope, ok := s.mockScopes[appID]
	if !ok || scope.AccountID != accountID {
		return nil, store.ErrAppNotFound
	}
	item := appCatalogItemFromTestScope(scope)
	return &item, nil
}

func (s *artifactReferenceGraphQLTestStore) ListAuthorizedAppsByFamily(_ context.Context, accountID, familyID uuid.UUID, _ accesscontrol.AuthorizedScope) ([]store.AppCatalogItem, error) {
	item, err := s.GetAuthorizedApp(context.Background(), accountID, familyID, accesscontrol.AuthorizedScope{All: true})
	if err != nil {
		return nil, err
	}
	return []store.AppCatalogItem{*item}, nil
}

func (s *artifactReferenceGraphQLTestStore) ListAuthorizedAppServiceSummaries(_ context.Context, _, _ uuid.UUID, _ accesscontrol.AuthorizedScope) ([]store.AppServiceSummary, error) {
	return s.services, nil
}

func appCatalogItemFromTestScope(scope *store.AppRuntime) store.AppCatalogItem {
	status := scope.Status
	if status == "" {
		status = "active"
	}
	return store.AppCatalogItem{AppFamilyID: scope.AppID, AppID: scope.AppID, Name: scope.Name,
		Version: scope.Version, Kind: scope.Kind, Status: status, CreatedAt: scope.CreatedAt}
}

func (s *artifactReferenceGraphQLTestStore) ResolveResourceReference(_ context.Context, query store.ResourceReferenceQuery) (uuid.UUID, error) {
	s.lastQuery = query
	if s.referenceErr != nil {
		return uuid.Nil, s.referenceErr
	}
	id, ok := s.references[query.Value]
	if !ok {
		return uuid.Nil, store.ErrResourceReferenceNotFound
	}
	return id, nil
}

func TestAppFamilyReferenceGraphQLSerializesSafeErrorCode(t *testing.T) {
	fixture := &workspaceTestStore{accountID: uuid.New()}
	s := &artifactReferenceGraphQLTestStore{workspaceTestStore: fixture, referenceErr: store.ErrResourceReferenceAmbiguous}
	h := mountMCPGraphQLTestHandler(t, s)
	req := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"query { appFamilyReference(reference: \"support\") { id } }"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	h(response, req)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":"FUSED_RESOURCE_AMBIGUOUS"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestAppReferenceGraphQLCarriesVersionSeparately(t *testing.T) {
	appID := uuid.New()
	fixture := &workspaceTestStore{accountID: uuid.New()}
	s := &artifactReferenceGraphQLTestStore{workspaceTestStore: fixture, references: map[string]uuid.UUID{"support": appID}}
	h := mountMCPGraphQLTestHandler(t, s)
	data := doMCPGraphQLRequest(t, h, `query { appReference(reference: "support", version: "2.0.0", kind: "sdk") { id kind } }`)
	if data["appReference"].(map[string]any)["id"] != appID.String() {
		t.Fatalf("app reference = %#v", data["appReference"])
	}
	if s.lastQuery.Kind != store.ReferenceApp || s.lastQuery.AppVersion != "2.0.0" || s.lastQuery.AppKind != "sdk" {
		t.Fatalf("reference query = %#v", s.lastQuery)
	}
}

func TestFetchWorkspaceServiceAuthOptions_PropagatesRegistryFailure(t *testing.T) {
	serviceID := uuid.New()
	verifier := &mockVerifier{authConfigErr: errors.New("registry unavailable")}
	_, err := fetchWorkspaceServiceAuthOptions(context.Background(), verifier, "fsk_test", []store.WorkspaceService{{
		ServiceID: serviceID,
		Version:   "1.0.0",
	}})
	if err == nil || !strings.Contains(err.Error(), "registry unavailable") {
		t.Fatalf("expected Registry auth metadata failure, got %v", err)
	}
}

// mountMCPGraphQLTestHandler builds the schema fresh per test (graphql-go
// schemas are cheap to construct and this avoids any cross-test state in the
// generated graphql.Schema value) and wires it to s the same way
// MountMCPGraphQLRoute does in production.
func mountMCPGraphQLTestHandler(t *testing.T, s store.Store) http.HandlerFunc {
	return mountMCPGraphQLTestHandlerWithRegistry(t, s, &mockRegistryClient{})
}

func mountMCPGraphQLTestHandlerWithRegistry(t *testing.T, s store.Store, registry sandbox.RegistryClient) http.HandlerFunc {
	return mountMCPGraphQLTestHandlerWithRegistryAndSink(t, s, registry, nil)
}

func mountMCPGraphQLTestHandlerWithRegistryAndSink(t *testing.T, s store.Store, registry sandbox.RegistryClient, revisionSink authorizationRevisionSink) http.HandlerFunc {
	t.Helper()
	configStore := &mockConfigStore{}
	if fixture, ok := s.(*workspaceTestStore); ok {
		configStore.appRuntimeSink = func(scope store.AppRuntime) error {
			return fixture.SaveAppRuntime(context.Background(), scope)
		}
	}
	schema, err := newMCPGraphQLSchema(configStore, s, &mockVerifier{}, registry, []byte("12345678901234567890123456789012"))
	if err != nil {
		t.Fatalf("newMCPGraphQLSchema() error = %v", err)
	}
	slugResolver, _ := registry.(sdkServiceSlugResolver)
	return withGraphQLTestOwner(t, s, mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: s, configStore: configStore, slugResolver: slugResolver, revisionSink: revisionSink}))
}

func withGraphQLTestOwner(t *testing.T, s store.Store, next http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	accountID := uuid.New()
	switch fixture := s.(type) {
	case *workspaceTestStore:
		accountID = fixture.accountID
	case *artifactReferenceGraphQLTestStore:
		accountID = fixture.accountID
	}
	workspaceID := uuid.New()
	grants := make([]accesscontrol.Grant, 0, len(accesscontrol.AllPermissions()))
	for _, permission := range accesscontrol.AllPermissions() {
		grants = append(grants, accesscontrol.Grant{
			Permission: permission,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
		})
	}
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
	if err != nil {
		t.Fatalf("build test authorization snapshot: %v", err)
	}
	actor := accesscontrol.Actor{AccountID: accountID, WorkspaceID: workspaceID, SubjectID: uuid.New(), Authorization: snapshot}
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(accesscontrol.ContextWithActor(r.Context(), actor)))
	}
}

func doMCPGraphQLRequest(t *testing.T, h http.HandlerFunc, query string) map[string]any {
	return doMCPGraphQLRequestWithVariables(t, h, query, nil)
}

func doMCPGraphQLRequestWithVariables(t *testing.T, h http.HandlerFunc, query string, variables map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	req := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	assertEngineGraphQLServerTiming(t, rr.Header().Get("Server-Timing"), true)
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	if errs, ok := resp["errors"]; ok {
		t.Fatalf("graphql errors: %v", errs)
	}
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %#v", resp)
	}
	return data
}

func TestMCPGraphQLHandler_RejectsUnauthenticated(t *testing.T) {
	s := &workspaceTestStore{workspaceErr: errWorkspaceNotFoundForTest{}}
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, s, &mockVerifier{}, &mockRegistryClient{}, []byte("12345678901234567890123456789012"))
	if err != nil {
		t.Fatalf("newMCPGraphQLSchema() error = %v", err)
	}
	h := mcpGraphQLHandler(schema)

	req := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(`{"query":"query { mcpServers(limit:1,offset:0) { total } }"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	assertEngineGraphQLServerTiming(t, rr.Header().Get("Server-Timing"), false)
}

func TestEngineGraphQLServiceActivityRejectsInactiveService(t *testing.T) {
	serviceID := uuid.New()
	s := &workspaceTestStore{accountID: uuid.New(), workspaceID: uuid.New()}
	h := mountMCPGraphQLTestHandler(t, s)
	body := `{"query":"query { webhookEvents(service_id: \"` + serviceID.String() + `\") { total } webhookAnalytics(service_id: \"` + serviceID.String() + `\") { total_ingested } engineExecutionEvents(service_id: \"` + serviceID.String() + `\") { total } engineExecutionAnalytics(service_id: \"` + serviceID.String() + `\") { total_calls } }"}`
	req := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "fsk_test")
	response := httptest.NewRecorder()

	h(response, req)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "service is not active in this workspace") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"total_ingested":`) || strings.Contains(response.Body.String(), `"total_calls":`) || strings.Contains(response.Body.String(), `"total":`) {
		t.Fatalf("inactive service returned activity data: %s", response.Body.String())
	}
}

func assertEngineGraphQLServerTiming(t *testing.T, timing string, wantExecution bool) {
	t.Helper()
	for _, want := range []string{"engine_auth;dur=", "engine_total;dur="} {
		if !strings.Contains(timing, want) {
			t.Fatalf("Server-Timing = %q, want metric %q", timing, want)
		}
	}
	if wantExecution && !strings.Contains(timing, "engine_graphql;dur=") {
		t.Fatalf("Server-Timing = %q, want GraphQL execution metric", timing)
	}
}

func TestEngineGraphQLRefreshMissingServiceContracts_BackfillsActivatedVersions(t *testing.T) {
	exporter := setupTestTracer(t)
	firstServiceID := uuid.New()
	firstVersionID := uuid.New()
	secondServiceID := uuid.New()
	secondVersionID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		missingContractVersions: []store.WorkspaceServiceVersion{
			{ServiceID: firstServiceID, ServiceVersionID: firstVersionID, Version: "2026-07-22", Status: "public"},
			{ServiceID: secondServiceID, ServiceVersionID: secondVersionID, Version: "2026-07-23", Status: "public"},
		},
	}
	registry := &runtimeContractRegistryClient{}
	h := mountMCPGraphQLTestHandlerWithRegistry(t, s, registry)

	data := doMCPGraphQLRequestWithVariables(t, h, `mutation Refresh($limit: Int!) {
		refreshMissingServiceContracts(limit: $limit) {
			status
			missing
			refreshed
			failed
			results { service_id service_version_id version contract_hash error }
		}
	}`, map[string]any{"limit": 2})

	payload := data["refreshMissingServiceContracts"].(map[string]any)
	if payload["missing"] != float64(2) || payload["refreshed"] != float64(2) || payload["failed"] != float64(0) {
		t.Fatalf("unexpected refresh payload: %#v", payload)
	}
	if s.missingContractLimit != 2 {
		t.Fatalf("expected GraphQL limit 2 to reach store, got %d", s.missingContractLimit)
	}
	if len(registry.batchRuntimeArgs) != 1 || len(registry.batchRuntimeArgs[0]) != 2 {
		t.Fatalf("expected one batched registry fetch, got %#v", registry.batchRuntimeArgs)
	}
	if len(s.snapshotWrites) != 2 {
		t.Fatalf("expected two snapshot writes, got %#v", s.snapshotWrites)
	}
	spans := exporter.GetSpans()
	if !hasSpanNamed(spans, "engine.workspace.refresh_missing_runtime_contracts") {
		t.Fatalf("expected missing-refresh span, got %#v", spans)
	}
}

func hasSpanNamed(spans []tracetest.SpanStub, name string) bool {
	for _, span := range spans {
		if span.Name == name {
			return true
		}
	}
	return false
}

type runtimeContractRegistryClient struct {
	mockRegistryClient
	batchRuntimeArgs [][]store.WorkspaceServiceVersion
	runtimeErr       error
}

func (m *runtimeContractRegistryClient) FetchRuntimeContracts(ctx context.Context, versions []store.WorkspaceServiceVersion, apiKey string) ([]store.ServiceContractSnapshot, error) {
	m.batchRuntimeArgs = append(m.batchRuntimeArgs, append([]store.WorkspaceServiceVersion(nil), versions...))
	if m.runtimeErr != nil {
		return nil, m.runtimeErr
	}
	out := make([]store.ServiceContractSnapshot, 0, len(versions))
	for _, version := range versions {
		out = append(out, store.ServiceContractSnapshot{
			ServiceID:        version.ServiceID,
			ServiceVersionID: version.ServiceVersionID,
			Version:          version.Version,
			ContractHash:     "hash-" + version.ServiceVersionID.String(),
		})
	}
	return out, nil
}

// TestEngineGraphQLConnectAuth_WorkspaceConnectConfigs covers the read path
// workspace sync needs: masked connect-config metadata plus its profile
// snapshots, in GraphQL. (This test used to also cover the standalone
// buckets/connectConfig/serviceConnectConfigs fields, removed as dead code --
// nothing in the UI, CLI, or e2e scripts called them; bucket listing goes
// through bucketSummaries/bucketSummaryPage instead, and per-service connect
// config reads have no live caller left now that workspace sync owns this
// read model.)
func TestEngineGraphQLConnectAuth_WorkspaceConnectConfigs(t *testing.T) {
	accountID := uuid.New()
	workspaceID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	now := time.Now().UTC()
	s := &workspaceTestStore{
		accountID:   accountID,
		workspaceID: workspaceID,
		workspaceConnectConfigs: []store.WorkspaceConnectConfig{{
			ConnectConfig: store.ConnectConfig{
				ID: uuid.New(), BucketID: bucketID, ServiceID: serviceID,
				AuthType: "oauth", AuthName: "primaryOAuth", Enabled: true, EncryptedClientID: "enc:id", EncryptedClientSecret: "enc:secret",
				RedirectURI: "http://localhost:8081/connect/callback", CreatedAt: now, UpdatedAt: now,
			},
			BucketName: "github",
		}},
		workspaceConnectProfiles: []store.WorkspaceConnectionProfile{{
			ID: uuid.New(), ServiceID: serviceID,
			ServiceVersionID: uuid.New(), AuthType: "oauth", Layer: "override", ProfileRevision: 1,
			ProfileHash: "profile-hash", Provenance: "workspace", ProfileSnapshot: []byte(`{"auth_type":"oauth","bindings":[]}`),
		}},
	}
	h := mountMCPGraphQLTestHandler(t, s)

	query := `query {
		workspaceConnectConfigs {
			bucket_id bucket_name service_id auth_type auth_name enabled redirect_uri has_client_id has_client_secret
			profiles { service_version_id auth_type provenance profile }
		}
	}`
	data := doMCPGraphQLRequest(t, h, query)

	workspaceConfigs := data["workspaceConnectConfigs"].([]any)
	if len(workspaceConfigs) != 1 {
		t.Fatalf("expected one workspace sync config, got %#v", workspaceConfigs)
	}
	exported := workspaceConfigs[0].(map[string]any)
	if exported["bucket_name"] != "github" || exported["auth_name"] != "primaryOAuth" || exported["has_client_secret"] != true {
		t.Fatalf("unexpected workspace sync config: %#v", exported)
	}
	profiles := exported["profiles"].([]any)
	if len(profiles) != 1 || profiles[0].(map[string]any)["provenance"] != "workspace" {
		t.Fatalf("expected safe profile snapshot in workspace sync config, got %#v", profiles)
	}
}

// TestEngineGraphQLConnectionResourcesListAndSetDefault exercises the UI/CLI
// read and audited mutation through an owned opaque connection ID.
func TestEngineGraphQLConnectionResourcesListAndSetDefault(t *testing.T) {
	accountID, bucketID := uuid.New(), uuid.New()
	connectionID, resourceID := uuid.New(), uuid.New()
	s := &workspaceTestStore{
		accountID:       accountID,
		buckets:         []store.Bucket{{ID: bucketID}},
		authConnections: []store.AuthConnection{{ID: connectionID, BucketID: bucketID}},
		connectionResources: map[uuid.UUID][]store.ConnectionResource{
			connectionID: {{ID: resourceID, ConnectionID: connectionID, BucketID: bucketID, ResourceType: "jira_site", DisplayName: "Acme", BaseURL: "https://api.atlassian.com/ex/jira/cloud-a"}},
		},
	}
	handler := mountMCPGraphQLTestHandler(t, s)
	data := doMCPGraphQLRequest(t, handler, `query { connectionResources(connection_id: "`+connectionID.String()+`") { id display_name resource_type base_url is_default } }`)
	resources, ok := data["connectionResources"].([]any)
	if !ok || len(resources) != 1 {
		t.Fatalf("unexpected resource list: %#v", data)
	}
	data = doMCPGraphQLRequest(t, handler, `mutation { setDefaultConnectionResource(connection_id: "`+connectionID.String()+`", resource_id: "`+resourceID.String()+`") { id is_default } }`)
	if data["setDefaultConnectionResource"] == nil || s.defaultConnectionResourceID != resourceID {
		t.Fatalf("default mutation failed: data=%#v selected=%s", data, s.defaultConnectionResourceID)
	}
}

func TestEngineGraphQLSDKBuckets_UsesLinkedRuntimeBucket(t *testing.T) {
	accountID := uuid.New()
	workspaceID := uuid.New()
	appID := uuid.New()
	mcpAppID := uuid.New()
	attachedBucketID := uuid.New()
	defaultBucketID := uuid.New()
	now := time.Now().UTC()
	attachedBucket := store.Bucket{
		ID: attachedBucketID, Name: "prod-users",
		IsDefault: false, CreatedAt: now, UpdatedAt: now,
	}
	valueID := uuid.New()
	secretID := uuid.New()
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	connectionID := uuid.New()
	tokenID := uuid.New()
	tokenLastUsedAt := now.Add(2 * time.Minute)
	s := &workspaceTestStore{
		accountID:   accountID,
		workspaceID: workspaceID,
		workspaceServices: []store.WorkspaceService{{
			ID: uuid.New(), ServiceID: serviceID,
			ServiceName: "Linear", Version: "2026-07-21", ServiceVersionID: serviceVersionID, AddedBy: accountID, CreatedAt: now,
		}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{
				ID: uuid.New(), ServiceID: serviceID,
				Version: "2026-07-21", ServiceVersionID: serviceVersionID, Status: "enabled",
				CreatedAt: now, EnabledAt: now,
			}},
		},
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			appID: {
				AccountID: accountID, AppID: appID, Kind: "sdk", Name: "jira-activity-smoke", Version: "1.0.0", CreatedAt: now,
				Selections: []byte(`[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","operation_names":["issues.list"]}]`),
			},
			mcpAppID: {
				AccountID: accountID, AppID: mcpAppID, Kind: "mcp", Name: "linear-tools", Version: "1.0.0", CreatedAt: now,
				Selections: []byte(`[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","operation_names":["issues.list"]}]`),
			},
		},
		listWorkspaceWebhooksResult: []store.WorkspaceWebhook{{
			ID: uuid.New(), ServiceID: serviceID, ServiceVersionID: serviceVersionID,
			Label: "repo", Slug: "repo", CreatedAt: now,
		}},
		webhookEvents: []models.WebhookEvent{{
			ID: uuid.New(), AccountID: accountID, ServiceID: serviceID, MsgID: "msg-1",
			EventType: "repo.created", VerificationStatus: "verified", DeliveryStatus: "success",
			LatencyMs: 27, RetryCount: 0, CreditsConsumed: 0.5, PayloadSize: 128, CreatedAt: now,
		}},
		webhookAnalytics: models.WebhookAnalytics{
			TotalIngested: 1, TotalDelivered: 1, TotalRejected: 0, TotalFailed: 0,
		},
		engineExecutionEvents: []models.EngineExecutionEvent{{
			ID: uuid.New(), AppFamilyID: appID, AppID: appID, AppVersion: "1.0.0", ServiceID: serviceID, ServiceVersionID: serviceVersionID.String(),
			Transport: models.EngineExecutionTransportSDK, EndpointName: "issues.list", HTTPMethod: "GET", RequestPath: "/issues",
			Environment: "production", EnvironmentSource: "provider", ProviderHost: "api.linear.app",
			Status: models.EngineExecutionStatusSuccess, LatencyMs: 41, StartedAt: now, EndedAt: now, CreatedAt: now,
		}, {
			ID: uuid.New(), AppFamilyID: mcpAppID, AppID: mcpAppID, AppVersion: "1.0.0", ServiceID: serviceID, ServiceVersionID: serviceVersionID.String(),
			Transport: models.EngineExecutionTransportMCP, EndpointName: "issues.list", HTTPMethod: "GET", RequestPath: "/issues",
			Environment: "production", EnvironmentSource: "provider", ProviderHost: "api.linear.app",
			Status: models.EngineExecutionStatusSuccess, LatencyMs: 52, StartedAt: now, EndedAt: now, CreatedAt: now,
		}},
		engineExecutionAnalytics: models.EngineExecutionAnalytics{
			TotalCalls: 1, SuccessfulCalls: 1, AverageLatencyMs: 41,
		},
		appExecutionAnalytics: models.AppExecutionAnalytics{
			EngineExecutionAnalytics: models.EngineExecutionAnalytics{
				TotalCalls: 1, SuccessfulCalls: 1, AverageLatencyMs: 41, P95LatencyMs: 41,
			},
			ByService: []models.EngineExecutionBreakdown{{
				Key: serviceID.String(), Label: serviceID.String(), TotalCalls: 1, P95LatencyMs: 41,
			}},
		},
		workspaceExecutionAnalytics: models.WorkspaceExecutionAnalytics{
			EngineExecutionAnalytics: models.EngineExecutionAnalytics{TotalCalls: 1, SuccessfulCalls: 1, P95LatencyMs: 41},
			ByTransport:              []models.EngineExecutionBreakdown{{Key: "sdk", Label: "SDK", TotalCalls: 1, P95LatencyMs: 41}},
		},
		appTokens: []store.AppTokenMetadata{{
			ID: tokenID, AppFamilyID: appID, Name: "agent",
			AppTokenPolicy: store.AppTokenPolicy{AllowedOperations: []string{"issues.list"}, ExpiresAt: &tokenLastUsedAt},
			LastUsedAt:     &tokenLastUsedAt, CreatedAt: now,
		}},
		appRuntimesForBucket: map[uuid.UUID][]store.AppRuntime{
			attachedBucketID: {{AccountID: accountID, AppID: appID, BucketID: attachedBucketID, Kind: "sdk", Name: "prod sdk", CreatedAt: now}},
		},
		buckets: []store.Bucket{{
			ID: defaultBucketID, Name: "default", IsDefault: true,
			CreatedAt: now, UpdatedAt: now,
		}, attachedBucket},
		bucketSummaries: []store.BucketSummary{
			{Bucket: attachedBucket, SecretCount: 1, ValueCount: 1},
		},
		sdkBuckets: map[uuid.UUID][]store.Bucket{appID: {attachedBucket}},
		bucketValues: map[uuid.UUID][]store.BucketValue{
			attachedBucketID: {{
				ID: valueID, BucketID: attachedBucketID, ServiceID: serviceID,
				KeyName: "X-Region", Location: "header", Value: "eu", CreatedAt: now, UpdatedAt: now,
			}},
		},
		secretMetas: map[uuid.UUID][]store.WorkspaceSecretMeta{
			attachedBucketID: {{
				ID: secretID, BucketID: attachedBucketID, ServiceID: serviceID,
				KeyName: "bearerAuth", KeyNames: []string{"bearerAuth"}, CredentialType: "bearer", CreatedAt: now, UpdatedAt: now,
			}},
		},
		bucketConnectSummaries: map[uuid.UUID]*store.BucketConnectSummary{
			attachedBucketID: {BucketID: attachedBucketID, ConnectConfigCount: 1, ConnectedUserCount: 2},
		},
		bucketServiceSummaries: map[uuid.UUID][]store.BucketServiceSummary{
			attachedBucketID: {{
				ServiceID: serviceID, ServiceName: "Linear", SecretCount: 1,
				ValueCount: 1, ConnectConfigCount: 1, ConnectedUserCount: 1,
			}, {
				ServiceID: uuid.New(), ServiceName: "Stripe", SecretCount: 1,
				ValueCount: 0, ConnectConfigCount: 0, ConnectedUserCount: 0,
			}},
		},
		authConnections: []store.AuthConnection{{
			ID: connectionID, BucketID: attachedBucketID, ServiceID: serviceID,
			EndUserRef: "user-123", CreatedByAppID: appID, AuthType: "oauth2", TokenType: "Bearer",
			Scopes: []string{"read"}, ScopeSource: "provider", RefreshState: "valid", CreatedAt: now, UpdatedAt: now,
		}},
	}
	verifier := &mockVerifier{
		visibilityOverrides: map[uuid.UUID]sandbox.ServiceVisibility{
			serviceID: {ServiceID: serviceID, IsOwner: true, Slug: "linear"},
		},
		authConfigVersions: []sandbox.ServiceVersionAuthConfigs{{
			ServiceID: serviceID, Version: serviceVersionID.String(), ServiceVersionID: serviceVersionID,
			AuthConfigs: fusedobject.AuthConfigs{{Name: "apiKeyAuth", Type: "apiKey", KeyName: "X-API-Key"}},
		}},
	}
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, s, verifier, &mockRegistryClient{}, []byte("12345678901234567890123456789012"))
	if err != nil {
		t.Fatalf("newMCPGraphQLSchema() error = %v", err)
	}
	h := withGraphQLTestOwner(t, s, mcpGraphQLHandler(schema, graphQLAuthorizationResources{store: s}))

	query := `query {
		bucketSummary(bucket_id: "` + attachedBucketID.String() + `") { id name secret_count value_count }
		bucketSummaryPage(limit: 10, offset: 0) { total items { id name secret_count value_count } }
		workspaceServices {
			service_id service_name service_slug version
			enabled_versions { version status }
			auth_options { label auth_type credential_type key_name key_prefix required_fields supports_connected_users }
		}
		workspaceServicePage(limit: 10, offset: 0) { total page limit data { service_id service_name service_slug version } }
		workspaceWebhooks(service_id: "` + serviceID.String() + `") { label slug created_at }
		webhookEvents(service_id: "` + serviceID.String() + `", event_name: "repo.created", limit: 10, offset: 0) { total items { msg_id event_name delivery_status latency_ms credits_consumed created_at } }
		webhookAnalytics(service_id: "` + serviceID.String() + `", event_name: "repo.created") { total_ingested total_delivered total_rejected total_failed }
		engineExecutionEvents(service_id: "` + serviceID.String() + `", transport: "sdk", status: "success", limit: 10, offset: 0) { total items { app_family_id app_id app_version app_kind transport operation http_method request_path environment environment_source provider_host status latency_ms started_at timings { name duration_ms } } }
		appExecutionEvents(app_id: "` + appID.String() + `", transport: "sdk", status: "success", limit: 10, offset: 0) { total items { app_family_id app_id app_version service_id transport operation http_method request_path provider_host status latency_ms } }
		appExecutionAnalytics(app_id: "` + appID.String() + `", transport: "sdk") { total_calls successful_calls failed_calls average_latency_ms p95_latency_ms by_service { key total_calls failed_calls p95_latency_ms } }
		mcpAppExecutionEvents: appExecutionEvents(app_id: "` + mcpAppID.String() + `", transport: "mcp", status: "success", limit: 10, offset: 0) { total items { app_id app_version transport operation status latency_ms } }
		engineExecutionAnalytics(service_id: "` + serviceID.String() + `", transport: "sdk", status: "success") { total_calls successful_calls failed_calls average_latency_ms }
		workspaceExecutionAnalytics { total_calls successful_calls failed_calls p95_latency_ms by_transport { key total_calls } }
		serviceConsumers(service_id: "` + serviceID.String() + `") { id name version kind active service_version_id select_all operation_count webhook_count created_at }
		appTokens(app_family_id: "` + appID.String() + `") { id app_family_id name allow expires_at created_at last_used_at }
		sdkBuckets(app_family_id: "` + appID.String() + `") { id name is_default }
		bucketSDKPage(bucket_id: "` + attachedBucketID.String() + `", limit: 10, offset: 0) { total items { id name kind active } }
		bucketServicePage(bucket_id: "` + attachedBucketID.String() + `", search: "Lin", limit: 10, offset: 0) { total items { service_id service_name secret_count value_count connect_config_count connected_user_count } }
		bucketValues(bucket_id: "` + attachedBucketID.String() + `") { id service_id key_name location value }
		bucketValuePage(bucket_id: "` + attachedBucketID.String() + `", limit: 10, offset: 0) { total items { id service_id key_name location value } }
		secretMetas(bucket_id: "` + attachedBucketID.String() + `") { id service_id key_name credential_type }
		secretMetaPage(bucket_id: "` + attachedBucketID.String() + `", limit: 10, offset: 0) { total items { id service_id key_name key_names credential_type } }
		authConnectionPage(bucket_id: "` + attachedBucketID.String() + `", service_id: "` + serviceID.String() + `", limit: 10, offset: 0) { total items { id service_id end_user_ref auth_type token_type refresh_state } }
		bucketConnectSummary(bucket_id: "` + attachedBucketID.String() + `") { bucket_id connect_config_count connected_user_count }
	}`
	data := doMCPGraphQLRequest(t, h, query)

	assertLinkedBucketGraphQLData(t, data, attachedBucketID)
	assertWorkspaceServiceGraphQLData(t, data, verifier)
	assertWebhookAndTokenGraphQLData(t, data)
	assertBucketUsageGraphQLData(t, data)
	assertSecretAndConnectGraphQLData(t, data)
}

func assertLinkedBucketGraphQLData(t *testing.T, data map[string]any, attachedBucketID uuid.UUID) {
	oneSummary := graphQLMap(t, data["bucketSummary"], "bucketSummary")
	assertGraphQLField(t, oneSummary, "id", attachedBucketID.String(), "bucketSummary")
	assertGraphQLField(t, oneSummary, "secret_count", float64(1), "bucketSummary")
	sdkBuckets := graphQLList(t, data["sdkBuckets"], "sdkBuckets")
	assertGraphQLLen(t, sdkBuckets, 1, "sdkBuckets")
	first := graphQLMap(t, sdkBuckets[0], "sdkBuckets[0]")
	assertGraphQLField(t, first, "id", attachedBucketID.String(), "sdkBuckets[0]")
	assertGraphQLField(t, first, "name", "prod-users", "sdkBuckets[0]")
	summaryPage := graphQLMap(t, data["bucketSummaryPage"], "bucketSummaryPage")
	assertGraphQLField(t, summaryPage, "total", float64(1), "bucketSummaryPage")
	summaries := graphQLList(t, summaryPage["items"], "bucketSummaryPage.items")
	assertGraphQLLen(t, summaries, 1, "bucketSummaryPage.items")
	summary := graphQLMap(t, summaries[0], "bucketSummaryPage.items[0]")
	assertGraphQLField(t, summary, "secret_count", float64(1), "bucketSummaryPage.items[0]")
	assertGraphQLField(t, summary, "value_count", float64(1), "bucketSummaryPage.items[0]")
}

func assertWorkspaceServiceGraphQLData(t *testing.T, data map[string]any, verifier *mockVerifier) {
	services := graphQLList(t, data["workspaceServices"], "workspaceServices")
	assertGraphQLLen(t, services, 1, "workspaceServices")
	service := graphQLMap(t, services[0], "workspaceServices[0]")
	assertGraphQLField(t, service, "service_slug", "linear", "workspaceServices[0]")
	workspaceSvcPage := graphQLMap(t, data["workspaceServicePage"], "workspaceServicePage")
	assertGraphQLField(t, workspaceSvcPage, "total", float64(1), "workspaceServicePage")
	assertGraphQLField(t, workspaceSvcPage, "page", float64(1), "workspaceServicePage")
	assertGraphQLField(t, workspaceSvcPage, "limit", float64(10), "workspaceServicePage")
	pageData := graphQLList(t, workspaceSvcPage["data"], "workspaceServicePage.data")
	assertGraphQLLen(t, pageData, 1, "workspaceServicePage.data")
	assertGraphQLField(t, graphQLMap(t, pageData[0], "workspaceServicePage.data[0]"), "service_slug", "linear", "workspaceServicePage.data[0]")
	enabledVersions := graphQLList(t, service["enabled_versions"], "workspaceServices[0].enabled_versions")
	assertGraphQLLen(t, enabledVersions, 1, "workspaceServices[0].enabled_versions")
	assertGraphQLField(t, graphQLMap(t, enabledVersions[0], "enabled_versions[0]"), "status", "enabled", "enabled_versions[0]")
	authOptions := graphQLList(t, service["auth_options"], "workspaceServices[0].auth_options")
	assertGraphQLLen(t, authOptions, 1, "workspaceServices[0].auth_options")
	authOption := graphQLMap(t, authOptions[0], "auth_options[0]")
	assertGraphQLField(t, authOption, "auth_type", "api_key", "auth_options[0]")
	assertGraphQLField(t, authOption, "key_name", "apiKeyAuth", "auth_options[0]")
	assertGraphQLLen(t, verifier.authConfigCalls, 1, "authConfigCalls")
	assertGraphQLLen(t, verifier.authConfigCalls[0], 1, "authConfigCalls[0]")
}

func assertWebhookAndTokenGraphQLData(t *testing.T, data map[string]any) {
	webhooks := graphQLList(t, data["workspaceWebhooks"], "workspaceWebhooks")
	assertGraphQLLen(t, webhooks, 1, "workspaceWebhooks")
	assertGraphQLField(t, graphQLMap(t, webhooks[0], "workspaceWebhooks[0]"), "label", "repo", "workspaceWebhooks[0]")
	webhookEvents := graphQLMap(t, data["webhookEvents"], "webhookEvents")
	assertGraphQLField(t, webhookEvents, "total", float64(1), "webhookEvents")
	event := graphQLMap(t, graphQLList(t, webhookEvents["items"], "webhookEvents.items")[0], "webhookEvents.items[0]")
	assertGraphQLField(t, event, "delivery_status", "success", "webhookEvents.items[0]")
	webhookAnalytics := graphQLMap(t, data["webhookAnalytics"], "webhookAnalytics")
	assertGraphQLField(t, webhookAnalytics, "total_ingested", float64(1), "webhookAnalytics")
	assertGraphQLField(t, webhookAnalytics, "total_delivered", float64(1), "webhookAnalytics")
	executionEvents := graphQLMap(t, data["engineExecutionEvents"], "engineExecutionEvents")
	assertGraphQLField(t, executionEvents, "total", float64(1), "engineExecutionEvents")
	execution := graphQLMap(t, graphQLList(t, executionEvents["items"], "engineExecutionEvents.items")[0], "engineExecutionEvents.items[0]")
	assertGraphQLField(t, execution, "operation", "issues.list", "engineExecutionEvents.items[0]")
	assertGraphQLField(t, execution, "app_id", execution["app_family_id"], "engineExecutionEvents.items[0]")
	assertGraphQLField(t, execution, "app_version", "1.0.0", "engineExecutionEvents.items[0]")
	assertGraphQLField(t, execution, "app_kind", "sdk", "engineExecutionEvents.items[0]")
	assertGraphQLField(t, execution, "transport", "sdk", "engineExecutionEvents.items[0]")
	assertGraphQLField(t, execution, "http_method", "GET", "engineExecutionEvents.items[0]")
	assertGraphQLField(t, execution, "request_path", "/issues", "engineExecutionEvents.items[0]")
	assertGraphQLField(t, execution, "provider_host", "api.linear.app", "engineExecutionEvents.items[0]")
	assertGraphQLField(t, execution, "latency_ms", float64(41), "engineExecutionEvents.items[0]")
	appEvents := graphQLMap(t, data["appExecutionEvents"], "appExecutionEvents")
	assertGraphQLField(t, appEvents, "total", float64(1), "appExecutionEvents")
	appEvent := graphQLMap(t, graphQLList(t, appEvents["items"], "appExecutionEvents.items")[0], "appExecutionEvents.items[0]")
	assertGraphQLField(t, appEvent, "app_id", execution["app_id"], "appExecutionEvents.items[0]")
	assertGraphQLField(t, appEvent, "operation", "issues.list", "appExecutionEvents.items[0]")
	appAnalytics := graphQLMap(t, data["appExecutionAnalytics"], "appExecutionAnalytics")
	assertGraphQLField(t, appAnalytics, "total_calls", float64(1), "appExecutionAnalytics")
	serviceUsage := graphQLList(t, appAnalytics["by_service"], "appExecutionAnalytics.by_service")
	assertGraphQLLen(t, serviceUsage, 1, "appExecutionAnalytics.by_service")
	assertGraphQLField(t, graphQLMap(t, serviceUsage[0], "appExecutionAnalytics.by_service[0]"), "key", appEvent["service_id"], "appExecutionAnalytics.by_service[0]")
	mcpAppEvents := graphQLMap(t, data["mcpAppExecutionEvents"], "mcpAppExecutionEvents")
	assertGraphQLField(t, mcpAppEvents, "total", float64(1), "mcpAppExecutionEvents")
	mcpAppEvent := graphQLMap(t, graphQLList(t, mcpAppEvents["items"], "mcpAppExecutionEvents.items")[0], "mcpAppExecutionEvents.items[0]")
	assertGraphQLField(t, mcpAppEvent, "transport", "mcp", "mcpAppExecutionEvents.items[0]")
	assertGraphQLField(t, mcpAppEvent, "latency_ms", float64(52), "mcpAppExecutionEvents.items[0]")
	consumers := graphQLList(t, data["serviceConsumers"], "serviceConsumers")
	assertGraphQLLen(t, consumers, 2, "serviceConsumers")
	consumerKinds := make(map[string]any, len(consumers))
	for index, item := range consumers {
		consumer := graphQLMap(t, item, fmt.Sprintf("serviceConsumers[%d]", index))
		consumerKinds[consumer["name"].(string)] = consumer["kind"]
		assertGraphQLField(t, consumer, "operation_count", float64(1), fmt.Sprintf("serviceConsumers[%d]", index))
	}
	if consumerKinds["jira-activity-smoke"] != "sdk" || consumerKinds["linear-tools"] != "mcp" {
		t.Fatalf("service consumer kinds = %#v", consumerKinds)
	}
	executionAnalytics := graphQLMap(t, data["engineExecutionAnalytics"], "engineExecutionAnalytics")
	assertGraphQLField(t, executionAnalytics, "total_calls", float64(1), "engineExecutionAnalytics")
	assertGraphQLField(t, executionAnalytics, "successful_calls", float64(1), "engineExecutionAnalytics")
	workspaceAnalytics := graphQLMap(t, data["workspaceExecutionAnalytics"], "workspaceExecutionAnalytics")
	assertGraphQLField(t, workspaceAnalytics, "total_calls", float64(1), "workspaceExecutionAnalytics")
	assertGraphQLLen(t, graphQLList(t, workspaceAnalytics["by_transport"], "workspaceExecutionAnalytics.by_transport"), 1, "workspaceExecutionAnalytics.by_transport")
	tokens := graphQLList(t, data["appTokens"], "appTokens")
	assertGraphQLLen(t, tokens, 1, "appTokens")
	token := graphQLMap(t, tokens[0], "appTokens[0]")
	assertGraphQLField(t, token, "name", "agent", "appTokens[0]")
	assertGraphQLNonEmpty(t, token, "expires_at", "appTokens[0]")
	assertGraphQLNonEmpty(t, token, "last_used_at", "appTokens[0]")
	allow := graphQLList(t, token["allow"], "appTokens[0].allow")
	assertGraphQLLen(t, allow, 1, "appTokens[0].allow")
	if allow[0] != "issues.list" {
		t.Fatalf("appTokens[0].allow = %#v, want issues.list", allow)
	}
}

func assertBucketUsageGraphQLData(t *testing.T, data map[string]any) {
	sdkPage := graphQLMap(t, data["bucketSDKPage"], "bucketSDKPage")
	assertGraphQLField(t, sdkPage, "total", float64(1), "bucketSDKPage")
	sdkUsage := graphQLMap(t, graphQLList(t, sdkPage["items"], "bucketSDKPage.items")[0], "bucketSDKPage.items[0]")
	assertGraphQLField(t, sdkUsage, "name", "prod sdk", "bucketSDKPage.items[0]")
	servicePage := graphQLMap(t, data["bucketServicePage"], "bucketServicePage")
	assertGraphQLField(t, servicePage, "total", float64(1), "bucketServicePage")
	serviceUsage := graphQLMap(t, graphQLList(t, servicePage["items"], "bucketServicePage.items")[0], "bucketServicePage.items[0]")
	assertGraphQLField(t, serviceUsage, "connected_user_count", float64(1), "bucketServicePage.items[0]")
	values := graphQLList(t, data["bucketValues"], "bucketValues")
	assertGraphQLLen(t, values, 1, "bucketValues")
	assertGraphQLField(t, graphQLMap(t, values[0], "bucketValues[0]"), "value", "eu", "bucketValues[0]")
	valuePage := graphQLMap(t, data["bucketValuePage"], "bucketValuePage")
	assertGraphQLField(t, valuePage, "total", float64(1), "bucketValuePage")
	pageValue := graphQLMap(t, graphQLList(t, valuePage["items"], "bucketValuePage.items")[0], "bucketValuePage.items[0]")
	assertGraphQLField(t, pageValue, "value", "eu", "bucketValuePage.items[0]")
}

func assertSecretAndConnectGraphQLData(t *testing.T, data map[string]any) {
	secrets := graphQLList(t, data["secretMetas"], "secretMetas")
	assertGraphQLLen(t, secrets, 1, "secretMetas")
	assertGraphQLField(t, graphQLMap(t, secrets[0], "secretMetas[0]"), "key_name", "bearerAuth", "secretMetas[0]")
	secretPage := graphQLMap(t, data["secretMetaPage"], "secretMetaPage")
	assertGraphQLField(t, secretPage, "total", float64(1), "secretMetaPage")
	secret := graphQLMap(t, graphQLList(t, secretPage["items"], "secretMetaPage.items")[0], "secretMetaPage.items[0]")
	assertGraphQLField(t, secret, "key_name", "bearerAuth", "secretMetaPage.items[0]")
	keyNames := graphQLList(t, secret["key_names"], "secretMetaPage.items[0].key_names")
	assertGraphQLLen(t, keyNames, 1, "secretMetaPage.items[0].key_names")
	assertGraphQLValue(t, keyNames[0], "bearerAuth", "secretMetaPage.items[0].key_names[0]")
	connectionPage := graphQLMap(t, data["authConnectionPage"], "authConnectionPage")
	assertGraphQLField(t, connectionPage, "total", float64(1), "authConnectionPage")
	connection := graphQLMap(t, graphQLList(t, connectionPage["items"], "authConnectionPage.items")[0], "authConnectionPage.items[0]")
	assertGraphQLField(t, connection, "end_user_ref", "user-123", "authConnectionPage.items[0]")
	connectSummary := graphQLMap(t, data["bucketConnectSummary"], "bucketConnectSummary")
	assertGraphQLField(t, connectSummary, "connect_config_count", float64(1), "bucketConnectSummary")
	assertGraphQLField(t, connectSummary, "connected_user_count", float64(2), "bucketConnectSummary")
}

func graphQLMap(t *testing.T, value any, label string) map[string]any {
	t.Helper()
	obj, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", label, value)
	}
	return obj
}

func graphQLList(t *testing.T, value any, label string) []any {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%s = %#v, want list", label, value)
	}
	return items
}

func assertGraphQLLen[T any](t *testing.T, items []T, want int, label string) {
	t.Helper()
	if len(items) != want {
		t.Fatalf("%s length = %d, want %d: %#v", label, len(items), want, items)
	}
}

func assertGraphQLField(t *testing.T, obj map[string]any, field string, want any, label string) {
	t.Helper()
	assertGraphQLValue(t, obj[field], want, label+"."+field)
}

func assertGraphQLValue(t *testing.T, got any, want any, label string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %#v, want %#v", label, got, want)
	}
}

func assertGraphQLNonEmpty(t *testing.T, obj map[string]any, field string, label string) {
	t.Helper()
	if obj[field] == "" {
		t.Fatalf("%s.%s should be non-empty", label, field)
	}
}

func TestEngineGraphQLSecrets_DeleteCredentialFamily(t *testing.T) {
	bucketID := uuid.New()
	serviceID := uuid.New()
	s := &workspaceTestStore{
		accountID: uuid.New(),
		buckets:   []store.Bucket{{ID: bucketID, Name: "credentials"}},
	}
	h := mountMCPGraphQLTestHandler(t, s)
	query := `mutation {
		deleteSecrets(
			bucket_id: "` + bucketID.String() + `",
			service_id: "` + serviceID.String() + `",
			key_names: ["basicAuth_username", "basicAuth_password"]
		)
	}`
	doMCPGraphQLRequest(t, h, query)
	if len(s.deletedSecretKeys) != 2 {
		t.Fatalf("expected one credential family deletion, got %#v", s.deletedSecretKeys)
	}
}

func TestEngineGraphQLSecrets_UpsertPairedBasicSecrets(t *testing.T) {
	bucketID := uuid.New()
	serviceID := uuid.New()
	s := &workspaceTestStore{accountID: uuid.New()}
	h := mountMCPGraphQLTestHandler(t, s)

	query := `mutation {
		upsertSecrets(
			bucket_id: "` + bucketID.String() + `",
			secrets: [
				{service_id: "` + serviceID.String() + `", key_name: "basicAuth_username", credential_type: "basic", value: "user"},
				{service_id: "` + serviceID.String() + `", key_name: "basicAuth_password", credential_type: "basic", value: "pass"}
			]
		)
	}`
	data := doMCPGraphQLRequest(t, h, query)

	if data["upsertSecrets"] != true {
		t.Fatalf("expected mutation success, got %#v", data)
	}
	if len(s.upsertedSecrets) != 2 {
		t.Fatalf("expected two paired secret writes, got %d", len(s.upsertedSecrets))
	}
	if decryptWorkspaceSecretForTest(t, s.upsertedSecrets[0]) != "user" {
		t.Fatal("basic username should be encrypted before Store sees it")
	}
}

// TestEngineGraphQLConnectAuth_ListAndDeleteConnections proves connection
// listing and deletion remain bucket/service scoped through GraphQL.
func TestEngineGraphQLConnectAuth_ListAndDeleteConnections(t *testing.T) {
	workspaceID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	otherServiceID := uuid.New()
	connID := uuid.New()
	now := time.Now().UTC()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: workspaceID,
		buckets: []store.Bucket{{
			ID: bucketID, Name: "github", IsDefault: true,
			CreatedAt: now, UpdatedAt: now,
		}},
		authConnections: []store.AuthConnection{
			{
				ID: connID, BucketID: bucketID, ServiceID: serviceID,
				EndUserRef: "creativeJoe", AuthType: "oauth", TokenType: "bearer", Scopes: []string{"user:email"},
				ScopeSource: "provider", RefreshState: "ok", LastFailureCode: "provider_unauthorized",
				LastFailureAt: &now, LastFailureTraceID: "trace-123", CreatedAt: now, UpdatedAt: now,
			},
			{
				ID: uuid.New(), BucketID: bucketID, ServiceID: otherServiceID,
				EndUserRef: "someoneElse", AuthType: "oauth", TokenType: "bearer", ScopeSource: "none",
				RefreshState: "ok", CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)

	query := `query {
		authConnections(bucket_id: "` + bucketID.String() + `", service_id: "` + serviceID.String() + `") {
			id
			end_user_ref
			scopes
			refresh_state
			last_failure_code
			last_failure_at
			last_failure_trace_id
		}
	}`
	data := doMCPGraphQLRequest(t, h, query)
	connections := data["authConnections"].([]any)
	if len(connections) != 1 {
		t.Fatalf("expected service-filtered connection list, got %#v", connections)
	}
	first := connections[0].(map[string]any)
	if first["end_user_ref"] != "creativeJoe" || first["last_failure_code"] != "provider_unauthorized" || first["last_failure_trace_id"] != "trace-123" {
		t.Fatalf("unexpected connection projection: %#v", first)
	}

	deleteQuery := `mutation {
		deleteAuthConnection(bucket_id: "` + bucketID.String() + `", connection_id: "` + connID.String() + `")
	}`
	doMCPGraphQLRequest(t, h, deleteQuery)
	if len(s.deletedAuthConnections) != 1 || s.deletedAuthConnections[0] != connID {
		t.Fatalf("delete did not scope through bucket: %#v", s.deletedAuthConnections)
	}
}

func TestDeployMcpServer_CreatesActiveScopeWithNameAndKind(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	s := &workspaceTestStore{
		accountID:                accountID,
		workspaceServices:        []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "Stripe"}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, ServiceVersionID: serviceVersionID, Version: "2026-07-01"}}},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|2026-07-01": {ServiceID: serviceID, Version: "2026-07-01", ServiceVersionID: serviceVersionID, Revision: 1},
	}}
	revisionSink := &revisionSyncSinkStub{}
	h := mountMCPGraphQLTestHandlerWithRegistryAndSink(t, s, registry, revisionSink)

	query := `mutation($config: EngineJSON!,$owner:String!) {
		deployMcpServer(config: $config,owner_team:$owner) {
			id
			name
			version
			active
			mcp_url
		}
	}`
	data := doMCPGraphQLRequestWithVariables(t, h, query, map[string]any{"owner": "platform", "config": map[string]any{
		"apiVersion": "fused/v1", "kind": "mcp", "name": "stripe-mcp", "version": "1.0.0", "bucket": "default",
		"services": map[string]any{"Stripe": map[string]any{"version": "2026-07-01", "operations": []string{"listCharges"}}},
	}})

	deployed, ok := data["deployMcpServer"].(map[string]any)
	if !ok {
		t.Fatalf("expected deployMcpServer object, got %#v", data)
	}
	if deployed["name"] != "stripe-mcp" {
		t.Errorf("name = %v, want stripe-mcp", deployed["name"])
	}
	if deployed["active"] != true {
		t.Errorf("active = %v, want true", deployed["active"])
	}
	mcpURL, _ := deployed["mcp_url"].(string)
	if !strings.Contains(mcpURL, "/mcp/") || !strings.Contains(mcpURL, "/sse") {
		t.Errorf("mcp_url = %q, want an /mcp/{id}/sse URL", mcpURL)
	}

	if len(s.savedScopes) != 1 {
		t.Fatalf("expected one saved scope, got %#v", s.savedScopes)
	}
	if s.savedScopes[0].kind != store.AppKindMCP || s.savedScopes[0].accountID != accountID || s.savedScopes[0].ownerTeamID != testAppOwnerTeamID {
		t.Errorf("expected kind=mcp for accountID %s, got %#v", accountID, s.savedScopes[0])
	}
	if revisionSink.revision != s.authorizationRevision || revisionSink.revision == 0 {
		t.Errorf("authorization revision = %d, want committed revision %d", revisionSink.revision, s.authorizationRevision)
	}
}

// TestDeployMcpServer_SelectAllSkipsEndpointIds mirrors how the SDK
// generation path (and config-as-code apply) already handle "select all"
// service endpoints: send select_all:true with no endpoint_ids and let the
// server resolve every endpoint from the local contract snapshot at
// MCP-session-fixture-build time, instead of requiring the caller to enumerate
// IDs up front.
func TestDeployMcpServer_SelectAllSkipsEndpointIds(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	s := &workspaceTestStore{
		accountID:                accountID,
		workspaceServices:        []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "Stripe"}},
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{serviceID: {{ServiceID: serviceID, ServiceVersionID: serviceVersionID, Version: "2026-07-01"}}},
	}
	registry := &mockRegistryClient{contractRevisions: map[string]sandbox.ServiceVersionRevision{
		serviceID.String() + "|2026-07-01": {ServiceID: serviceID, Version: "2026-07-01", ServiceVersionID: serviceVersionID, Revision: 1},
	}}
	h := mountMCPGraphQLTestHandlerWithRegistry(t, s, registry)

	query := `mutation($config: EngineJSON!,$owner:String!) {
		deployMcpServer(config: $config,owner_team:$owner) {
			id
			active
		}
	}`
	doMCPGraphQLRequestWithVariables(t, h, query, map[string]any{"owner": "platform", "config": map[string]any{
		"apiVersion": "fused/v1", "kind": "mcp", "name": "stripe-mcp", "version": "1.0.0", "bucket": "default",
		"services": map[string]any{"Stripe": map[string]any{"version": "2026-07-01", "operations": []string{}, "select_all": true}},
	}})

	if len(s.savedScopes) != 1 {
		t.Fatalf("expected one saved scope, got %#v", s.savedScopes)
	}
	var selections []models.SDKSelection
	if err := json.Unmarshal(s.savedScopes[0].selections, &selections); err != nil {
		t.Fatalf("decode saved selections: %v", err)
	}
	if len(selections) != 1 || !selections[0].SelectAll {
		t.Errorf("expected a single select_all selection, got %#v", selections)
	}
	if len(selections[0].EndpointIDs) != 0 {
		t.Errorf("expected no endpoint_ids for a select_all selection, got %#v", selections[0].EndpointIDs)
	}
}

func TestMcpServers_ListsOnlyMCPKindScopesForAccount(t *testing.T) {
	accountID := uuid.New()
	otherAccountID := uuid.New()
	mcpID, appID, otherAccountMCPID := uuid.New(), uuid.New(), uuid.New()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			mcpID:             {AccountID: accountID, AppID: mcpID, Kind: "mcp", Name: "stripe-mcp", CreatedAt: time.Now()},
			appID:             {AccountID: accountID, AppID: appID, Kind: "sdk", Name: "stripe-sdk", CreatedAt: time.Now()},
			otherAccountMCPID: {AccountID: otherAccountID, AppID: otherAccountMCPID, Kind: "mcp", Name: "not-mine", CreatedAt: time.Now()},
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)

	data := doMCPGraphQLRequest(t, h, `query { mcpServers(limit: 10, offset: 0) { total items { id name } } }`)

	result, ok := data["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcpServers object, got %#v", data)
	}
	if result["total"] != float64(1) {
		t.Fatalf("total = %v, want 1 (only this account's mcp-kind scope)", result["total"])
	}
	items, _ := result["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %#v, want exactly one", items)
	}
	item := items[0].(map[string]any)
	if item["id"] != mcpID.String() || item["name"] != "stripe-mcp" {
		t.Errorf("unexpected item: %#v", item)
	}
}

func TestAppsListSDKAndMCPVersionsForAccount(t *testing.T) {
	accountID, otherAccountID := uuid.New(), uuid.New()
	sdkID, mcpID, hiddenID := uuid.New(), uuid.New(), uuid.New()
	fixture := &workspaceTestStore{accountID: accountID, mockScopes: map[uuid.UUID]*store.AppRuntime{
		sdkID:    {AccountID: accountID, AppID: sdkID, Kind: "sdk", Name: "support", Version: "1.0.0", CreatedAt: time.Now()},
		mcpID:    {AccountID: accountID, AppID: mcpID, Kind: "mcp", Name: "support-agent", Version: "2.0.0", CreatedAt: time.Now().Add(-time.Minute)},
		hiddenID: {AccountID: otherAccountID, AppID: hiddenID, Kind: "sdk", Name: "other-workspace", Version: "1.0.0", CreatedAt: time.Now().Add(-time.Hour)},
	}}
	s := &artifactReferenceGraphQLTestStore{workspaceTestStore: fixture}
	h := mountMCPGraphQLTestHandler(t, s)
	data := doMCPGraphQLRequest(t, h, `query { apps(limit: 20, offset: 0) { total items { app_id app_family_id name version kind status } } }`)
	page := data["apps"].(map[string]any)
	if page["total"] != float64(2) {
		t.Fatalf("app total = %#v, want 2", page["total"])
	}
	items := page["items"].([]any)
	kinds := map[string]bool{}
	for _, raw := range items {
		item := raw.(map[string]any)
		kinds[fmt.Sprint(item["kind"])] = true
		if item["app_id"] == "" || item["app_family_id"] == "" {
			t.Fatalf("app identity is incomplete: %#v", item)
		}
	}
	if len(items) != 2 || !kinds["sdk"] || !kinds["mcp"] {
		t.Fatalf("unexpected app items: %#v", items)
	}
}

func TestAppReadsExactEngineVersion(t *testing.T) {
	accountID, appID := uuid.New(), uuid.New()
	fixture := &workspaceTestStore{accountID: accountID, mockScopes: map[uuid.UUID]*store.AppRuntime{
		appID: {AccountID: accountID, AppID: appID, Kind: "sdk", Name: "support", Version: "2.0.0", CreatedAt: time.Now()},
	}}
	s := &artifactReferenceGraphQLTestStore{workspaceTestStore: fixture, references: map[string]uuid.UUID{"support@2.0.0": appID}}
	h := mountMCPGraphQLTestHandler(t, s)
	data := doMCPGraphQLRequest(t, h, `query { app(app_id: "`+appID.String()+`") { app_id app_family_id name version kind status } }`)
	app := data["app"].(map[string]any)
	if app["app_id"] != appID.String() || app["name"] != "support" || app["kind"] != "sdk" {
		t.Fatalf("unexpected app: %#v", app)
	}
}

func TestAppSelectionFieldsPreserveSyncDefinition(t *testing.T) {
	serviceID, serviceVersionID, endpointID := uuid.New(), uuid.New(), uuid.New()
	fields := appSelectionFields(store.AppCatalogItem{Selections: []models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: serviceVersionID, DefinitionSchemaVersion: 3,
		EndpointIDs: []uuid.UUID{endpointID}, OperationNames: []string{"createIssue"},
		AuthType: "oauth", AuthName: "atlassian", ConnectScopes: []string{"write:jira-work"},
		RequiredAuth: []models.SDKRequiredAuth{{AuthType: "oauth", AuthName: "atlassian"}, {AuthType: "mtls", AuthName: "clientCertificate"}},
		Injections:   []models.SDKInjectionConfig{{Location: "header", Name: "X-Tenant", Value: "${TENANT}", Mode: "template"}},
	}}})
	if len(fields) != 1 || fields[0]["definition_schema_version"] != 3 || fields[0]["auth_name"] != "atlassian" {
		t.Fatalf("selection metadata was not preserved: %#v", fields)
	}
	required := fields[0]["required_auth"].([]map[string]interface{})
	if len(required) != 2 || required[1]["auth_name"] != "clientCertificate" {
		t.Fatalf("selection required auth was not preserved: %#v", required)
	}
	injections := fields[0]["injections"].([]map[string]interface{})
	if len(injections) != 1 || injections[0]["name"] != "X-Tenant" {
		t.Fatalf("selection injections were not preserved: %#v", injections)
	}
}

func TestAppServicesUsesOnePermissionScopedAppLookup(t *testing.T) {
	accountID, appID, serviceID := uuid.New(), uuid.New(), uuid.New()
	fixture := &workspaceTestStore{accountID: accountID, mockScopes: map[uuid.UUID]*store.AppRuntime{
		appID: {AccountID: accountID, AppID: appID, Kind: "mcp", Name: "support", Version: "2.0.0"},
	}}
	s := &artifactReferenceGraphQLTestStore{
		workspaceTestStore: fixture,
		references:         map[string]uuid.UUID{"support": appID},
		services: []store.AppServiceSummary{{
			ServiceID: serviceID, ServiceSlug: "github", ServiceName: "GitHub", Version: "2026-08-01", SelectAll: true,
		}},
	}
	h := mountMCPGraphQLTestHandler(t, s)
	data := doMCPGraphQLRequest(t, h, `query { appServices(app_id: "`+appID.String()+`") { service_id service_slug version select_all endpoint_count } }`)
	services := data["appServices"].([]any)
	if len(services) != 1 || services[0].(map[string]any)["service_slug"] != "github" {
		t.Fatalf("unexpected artifact services: %#v", services)
	}
}

func TestDeprecateAppMutationKeepsVersionAvailable(t *testing.T) {
	accountID := uuid.New()
	appID := uuid.New()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			appID: {AccountID: accountID, AppID: appID, Kind: "mcp", Name: "stripe-mcp"},
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)
	data := doMCPGraphQLRequest(t, h, `mutation { deprecateApp(app_id: "`+appID.String()+`", message: "Use v2") }`)
	if data["deprecateApp"] != true || s.mockScopes[appID] == nil {
		t.Fatalf("deprecation removed app: data=%#v scopes=%#v", data, s.mockScopes)
	}
}

func TestUndeprecateAppMutationRestoresActiveStatus(t *testing.T) {
	accountID := uuid.New()
	appID := uuid.New()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			appID: {AccountID: accountID, AppID: appID, Kind: "mcp", Name: "stripe-mcp", Status: "deprecated"},
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)

	data := doMCPGraphQLRequest(t, h, `mutation { undeprecateApp(app_id: "`+appID.String()+`") }`)
	if data["undeprecateApp"] != true || s.mockScopes[appID].Status != "active" {
		t.Fatalf("undeprecate failed: data=%#v scope=%#v", data, s.mockScopes[appID])
	}
}

func TestDeactivateAppMutationPermanentlyRemovesVersion(t *testing.T) {
	accountID := uuid.New()
	appID := uuid.New()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			appID: {AccountID: accountID, AppID: appID, Kind: "mcp", Name: "stripe-mcp"},
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)

	data := doMCPGraphQLRequest(t, h, `mutation { deactivateApp(app_id: "`+appID.String()+`") }`)
	if data["deactivateApp"] != true {
		t.Errorf("deactivateApp = %v, want true", data["deactivateApp"])
	}
	if len(s.deletedScopes) != 1 || s.deletedScopes[0] != appID {
		t.Errorf("expected exact app deletion for %s, got %#v", appID, s.deletedScopes)
	}
}

func TestDeactivateAppMutationRejectsAnotherAccount(t *testing.T) {
	accountID := uuid.New()
	otherAccountID := uuid.New()
	appID := uuid.New()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			appID: {AccountID: otherAccountID, AppID: appID, Kind: "mcp"},
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)

	body, _ := json.Marshal(map[string]string{"query": `mutation { deactivateApp(app_id: "` + appID.String() + `") }`})
	req := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "permission_denied") {
		t.Fatalf("cross-account response = %d %s", rr.Code, rr.Body.String())
	}
	if len(s.deletedScopes) != 0 {
		t.Errorf("expected app not to be deleted, got %#v", s.deletedScopes)
	}
}

func TestMcpAnalytics_ReturnsDashboardForOwnedScope(t *testing.T) {
	accountID := uuid.New()
	appID := uuid.New()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			appID: {AccountID: accountID, AppID: appID, Kind: "mcp"},
		},
		mcpAnalyticsDashboard: &models.MCPAnalyticsDashboard{
			TotalRequests: 42, FailedRequests: 2, AverageLatencyMs: 123.5, ActiveAgents: 3,
			ToolUsage: []models.MCPToolUsage{{ToolName: "listUsers", Count: 40, Failed: 2, AverageLatencyMs: 100}},
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)

	data := doMCPGraphQLRequest(t, h, `query { mcpAnalytics(app_id: "`+appID.String()+`") {
		total_requests failed_requests active_agents
		tool_usage { tool_name count failed average_latency }
	} }`)

	dashboard, ok := data["mcpAnalytics"].(map[string]any)
	if !ok {
		t.Fatalf("expected mcpAnalytics object, got %#v", data)
	}
	if dashboard["total_requests"] != float64(42) || dashboard["active_agents"] != float64(3) {
		t.Errorf("unexpected totals: %#v", dashboard)
	}
	toolUsage, _ := dashboard["tool_usage"].([]any)
	if len(toolUsage) != 1 {
		t.Fatalf("tool_usage = %#v, want one entry", toolUsage)
	}
	entry := toolUsage[0].(map[string]any)
	if entry["tool_name"] != "listUsers" || entry["count"] != float64(40) {
		t.Errorf("unexpected tool usage entry: %#v", entry)
	}
}

func TestMcpAnalytics_RejectsAnotherAccountsScope(t *testing.T) {
	accountID := uuid.New()
	otherAccountID := uuid.New()
	appID := uuid.New()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			appID: {AccountID: otherAccountID, AppID: appID, Kind: "mcp"},
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)

	body, _ := json.Marshal(map[string]string{"query": `query { mcpAnalytics(app_id: "` + appID.String() + `") { total_requests } }`})
	req := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "permission_denied") {
		t.Fatalf("cross-account response = %d %s", rr.Code, rr.Body.String())
	}
}

func TestAppExecutionActivityRejectsAnotherAccount(t *testing.T) {
	accountID := uuid.New()
	appID := uuid.New()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			appID: {AccountID: uuid.New(), AppID: appID, Kind: "sdk"},
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)
	query := `query {
		events: appExecutionEvents(app_id: "` + appID.String() + `") { total }
		analytics: appExecutionAnalytics(app_id: "` + appID.String() + `") { total_calls }
	}`
	body, _ := json.Marshal(map[string]string{"query": query})
	req := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "permission_denied") {
		t.Fatalf("status/body = %d/%s, want permission denial", rr.Code, rr.Body.String())
	}
}

func TestAppExecutionActivitySelectsExactOrAllFamilyVersions(t *testing.T) {
	accountID := uuid.New()
	familyID := uuid.New()
	v1ID := uuid.New()
	v2ID := uuid.New()
	now := time.Now()
	s := &workspaceTestStore{
		accountID: accountID,
		apps: map[uuid.UUID]store.App{
			v1ID: {AppID: v1ID, AppFamilyID: familyID, AccountID: accountID, Version: "1.0.0", Status: "active"},
			v2ID: {AppID: v2ID, AppFamilyID: familyID, AccountID: accountID, Version: "2.0.0", Status: "active"},
		},
		engineExecutionEvents: []models.EngineExecutionEvent{
			{ID: uuid.New(), AccountID: accountID, AppFamilyID: familyID, AppID: v1ID, AppVersion: "1.0.0", Transport: "sdk", Status: "success", StartedAt: now, EndedAt: now},
			{ID: uuid.New(), AccountID: accountID, AppFamilyID: familyID, AppID: v2ID, AppVersion: "2.0.0", Transport: "sdk", Status: "success", StartedAt: now, EndedAt: now},
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)
	data := doMCPGraphQLRequest(t, h, `query {
		exact: appExecutionEvents(app_id: "`+v1ID.String()+`", limit: 10, offset: 0) { total }
		family: appExecutionEvents(app_id: "`+v1ID.String()+`", include_all_versions: true, limit: 10, offset: 0) { total }
	}`)
	assertGraphQLField(t, graphQLMap(t, data["exact"], "exact"), "total", float64(1), "exact")
	assertGraphQLField(t, graphQLMap(t, data["family"], "family"), "total", float64(2), "family")
}

func TestAppExecutionActivityReadsTombstonedAppIdentity(t *testing.T) {
	accountID, familyID, deletedAppID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now()
	fixture := &workspaceTestStore{
		accountID: accountID,
		tombstoneApps: map[uuid.UUID]store.App{
			deletedAppID: {
				AppID: deletedAppID, AppFamilyID: familyID, AccountID: accountID,
				Version: "1.0.0", SourceHash: "sha256:deleted",
			},
		},
		engineExecutionEvents: []models.EngineExecutionEvent{{
			ID: uuid.New(), AccountID: accountID, AppFamilyID: familyID, AppID: deletedAppID,
			AppVersion: "1.0.0", Transport: "sdk", Status: "success", StartedAt: now, EndedAt: now,
		}},
		appExecutionAnalytics: models.AppExecutionAnalytics{
			EngineExecutionAnalytics: models.EngineExecutionAnalytics{TotalCalls: 1},
		},
	}
	s := &artifactReferenceGraphQLTestStore{workspaceTestStore: fixture}
	h := mountMCPGraphQLTestHandler(t, s)
	data := doMCPGraphQLRequest(t, h, `query {
		apps(limit: 20, offset: 0) { total }
		activity: appExecutionEvents(app_id: "`+deletedAppID.String()+`", limit: 10, offset: 0) {
			total items { app_family_id app_id app_version }
		}
		analytics: appExecutionAnalytics(app_id: "`+deletedAppID.String()+`") { total_calls }
	}`)
	assertGraphQLField(t, graphQLMap(t, data["apps"], "apps"), "total", float64(0), "apps")
	activity := graphQLMap(t, data["activity"], "activity")
	assertGraphQLField(t, activity, "total", float64(1), "activity")
	event := graphQLMap(t, graphQLList(t, activity["items"], "activity.items")[0], "activity.items[0]")
	assertGraphQLField(t, event, "app_family_id", familyID.String(), "activity.items[0]")
	assertGraphQLField(t, event, "app_id", deletedAppID.String(), "activity.items[0]")
	assertGraphQLField(t, graphQLMap(t, data["analytics"], "analytics"), "total_calls", float64(1), "analytics")
}

func TestAppExecutionActivityRejectsAnotherAccountsTombstone(t *testing.T) {
	accountID, deletedAppID := uuid.New(), uuid.New()
	s := &workspaceTestStore{
		accountID: accountID,
		tombstoneApps: map[uuid.UUID]store.App{
			deletedAppID: {AppID: deletedAppID, AppFamilyID: uuid.New(), AccountID: uuid.New(), Version: "1.0.0"},
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)
	body, _ := json.Marshal(map[string]string{"query": `query { appExecutionEvents(app_id: "` + deletedAppID.String() + `") { total } }`})
	req := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "permission_denied") {
		t.Fatalf("status/body = %d/%s, want permission denial", rr.Code, rr.Body.String())
	}
}

// errWorkspaceNotFoundForTest is a stand-in auth failure for tests that only
// care that the handler rejects the request, not the exact error message.
type errWorkspaceNotFoundForTest struct{}

func (errWorkspaceNotFoundForTest) Error() string { return "workspace not found (test)" }

func TestMCPServerByName_ResolvesCorrectScope(t *testing.T) {
	accountID := uuid.New()
	appID := uuid.New()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			appID: {AccountID: accountID, AppID: appID, Kind: "mcp", Name: "my-mcp", Version: "1.0.0"},
		},
		getMCPScopeByNameFunc: func(ctx context.Context, acct uuid.UUID, name, version string) (*store.AppRuntime, error) {
			if acct == accountID && name == "my-mcp" && (version == "" || version == "1.0.0") {
				scope := store.AppRuntime{AccountID: accountID, AppID: appID, Kind: "mcp", Name: "my-mcp", Version: "1.0.0"}
				return &scope, nil
			}
			return nil, store.ErrAppRuntimeNotFound
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)

	t.Run("without version", func(t *testing.T) {
		data := doMCPGraphQLRequest(t, h, `query { mcpServerByName(name: "my-mcp") { id name version } }`)
		server := data["mcpServerByName"].(map[string]any)
		if server["id"] != appID.String() || server["name"] != "my-mcp" || server["version"] != "1.0.0" {
			t.Errorf("unexpected mcpServerByName result: %#v", server)
		}
	})

	t.Run("with version", func(t *testing.T) {
		data := doMCPGraphQLRequest(t, h, `query { mcpServerByName(name: "my-mcp", version: "1.0.0") { id name version } }`)
		server := data["mcpServerByName"].(map[string]any)
		if server["id"] != appID.String() {
			t.Errorf("unexpected id: %v", server["id"])
		}
	})
}

func TestMCPServerByName_NotFound(t *testing.T) {
	s := &workspaceTestStore{
		accountID: uuid.New(),
		getMCPScopeByNameFunc: func(ctx context.Context, acct uuid.UUID, name, version string) (*store.AppRuntime, error) {
			return nil, store.ErrAppRuntimeNotFound
		},
	}
	h := mountMCPGraphQLTestHandler(t, s)

	body, _ := json.Marshal(map[string]any{"query": `query { mcpServerByName(name: "nonexistent") { id } }`})
	req := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	errorsList, ok := resp["errors"].([]any)
	if !ok || len(errorsList) == 0 {
		t.Fatalf("expected errors list, got %#v", resp)
	}
	errObj := errorsList[0].(map[string]any)
	if !strings.Contains(errObj["message"].(string), "no MCP server found with name nonexistent") {
		t.Errorf("unexpected error message: %v", errObj["message"])
	}
}
