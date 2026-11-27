package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// doHybridGraphQLProxyRequest dispatches an authorized POST /graphql request through GraphQLProxyHandler.
func doHybridGraphQLProxyRequest(t *testing.T, handler http.Handler, actor accesscontrol.Actor, query string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query})
	// Fail immediately if test GraphQL JSON encoding fails.
	if err != nil {
		t.Fatalf("marshal GraphQL request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(accesscontrol.ContextWithActor(req.Context(), actor))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestHybridRegistryProxyGraphQLFallbackAndCacheIsolation verifies Local-first GraphQL fallback, permission checks, and cache partitioning.
func TestHybridRegistryProxyGraphQLFallbackAndCacheIsolation(t *testing.T) {
	var localCalls, cloudCalls atomic.Int32
	localRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localCalls.Add(1)
		var payload struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		// Simulate an internal spec hit for local-crm and an empty array miss for linear.
		if bytes.Contains([]byte(payload.Query), []byte("local-crm")) {
			_, _ = w.Write([]byte(`{"data":{"searchServices":[{"id":"local-1","name":"Local CRM"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"searchServices":[]}}`))
	}))
	defer localRegistry.Close()

	cloudRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudCalls.Add(1)
		_, _ = w.Write([]byte(`{"data":{"searchServices":[{"id":"cloud-1","name":"Linear"}]}}`))
	}))
	defer cloudRegistry.Close()

	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{Plan: "enterprise", LocalRegistryEnabled: true})
	defer entitlement.LiveEntitlement.Reset()

	hybrid := NewHybridRegistryProxy(
		NewRegistryProxy(cloudRegistry.URL, "fk_com_ent"),
		NewRegistryProxy(localRegistry.URL, "fk_com_ent"),
		nil,
	)
	handler := GraphQLProxyHandler(hybrid, nil)
	workspaceID := uuid.New()

	adminActor := actorWithWorkspacePermissions(t, workspaceID, accesscontrol.PermissionCatalogueRead, accesscontrol.PermissionCatalogueCloudRead)
	builderActor := actorWithWorkspacePermissions(t, workspaceID, accesscontrol.PermissionCatalogueRead)

	// 1. Local hit returns from Local Registry without touching Cloud.
	resLocal := doHybridGraphQLProxyRequest(t, handler, adminActor, `query { searchServices(query: "local-crm") { id name } }`)
	if resLocal.Code != http.StatusOK || resLocal.Header().Get(RegistrySourceHeader) != sandbox.RegistrySourceLocal || cloudCalls.Load() != 0 {
		t.Fatalf("expected local hit without cloud call, status=%d source=%q cloudCalls=%d", resLocal.Code, resLocal.Header().Get(RegistrySourceHeader), cloudCalls.Load())
	}

	// 2. Local miss by Admin (has catalogue.cloud.read) falls back to Cloud Registry.
	resCloud := doHybridGraphQLProxyRequest(t, handler, adminActor, `query { searchServices(query: "linear") { id name } }`)
	if resCloud.Code != http.StatusOK || resCloud.Header().Get(RegistrySourceHeader) != sandbox.RegistrySourceCloud || cloudCalls.Load() != 1 || !bytes.Contains(resCloud.Body.Bytes(), []byte("Linear")) {
		t.Fatalf("expected cloud fallback hit for admin, status=%d body=%s", resCloud.Code, resCloud.Body.String())
	}

	// 3. Identical query by Builder (lacks catalogue.cloud.read) must NOT read Admin's Cloud cache or call Cloud.
	resBuilder := doHybridGraphQLProxyRequest(t, handler, builderActor, `query { searchServices(query: "linear") { id name } }`)
	if resBuilder.Code != http.StatusOK || cloudCalls.Load() != 1 || bytes.Contains(resBuilder.Body.Bytes(), []byte("Linear")) {
		t.Fatalf("builder leaked cloud result: status=%d cloudCalls=%d body=%s", resBuilder.Code, cloudCalls.Load(), resBuilder.Body.String())
	}
}

type hybridMixedSDKStore struct {
	store.Store
	sources   map[uuid.UUID]string
	contracts []models.InlineSDKServiceContract
}

// GetWorkspaceServiceRegistrySources returns configured service provenance for mixed SDK proxy tests.
func (s *hybridMixedSDKStore) GetWorkspaceServiceRegistrySources(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(ids))
	for _, id := range ids {
		// Copy configured provenance entries for requested service IDs.
		if src, ok := s.sources[id]; ok {
			out[id] = src
		}
	}
	return out, nil
}

// SetWorkspaceServiceRegistrySource satisfies WorkspaceServiceRegistryStore in unit tests.
func (s *hybridMixedSDKStore) SetWorkspaceServiceRegistrySource(_ context.Context, serviceID uuid.UUID, source string) error {
	s.sources[serviceID] = source
	return nil
}

// BuildInlineSDKServiceContracts returns canned inline contracts for Cloud-pinned services.
func (s *hybridMixedSDKStore) BuildInlineSDKServiceContracts(_ context.Context, _ []store.ServiceContractMetadataRef) ([]models.InlineSDKServiceContract, error) {
	return s.contracts, nil
}

// TestHybridRegistryProxyMixedSDKAttachesInlineContracts verifies mixed Local+Cloud SDK builds route to Local Registry with inline Cloud snapshots.
func TestHybridRegistryProxyMixedSDKAttachesInlineContracts(t *testing.T) {
	localSvc := uuid.New()
	cloudSvc := uuid.New()
	cloudVer := uuid.New()
	var localReceived models.SDKGenerationRequest
	var cloudCalled atomic.Int32

	localRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&localReceived)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"job-1","status":"pending"}`))
	}))
	defer localRegistry.Close()

	cloudRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudCalled.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer cloudRegistry.Close()

	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{Plan: "enterprise", LocalRegistryEnabled: true})
	defer entitlement.LiveEntitlement.Reset()

	mockStore := &hybridMixedSDKStore{
		sources: map[uuid.UUID]string{
			localSvc: sandbox.RegistrySourceLocal,
			cloudSvc: sandbox.RegistrySourceCloud,
		},
		contracts: []models.InlineSDKServiceContract{{
			ServiceID:        cloudSvc,
			ServiceVersionID: cloudVer,
			Version:          "v1",
			Name:             "Linear",
		}},
	}
	hybrid := NewHybridRegistryProxy(
		NewRegistryProxy(cloudRegistry.URL, "fk_com_ent"),
		NewRegistryProxy(localRegistry.URL, "fk_com_ent"),
		mockStore,
	)

	payload, _ := json.Marshal(models.SDKGenerationRequest{
		Name: "mixed-sdk",
		Selections: []models.SDKSelection{
			{ServiceID: localSvc, ServiceVersionID: uuid.New()},
			{ServiceID: cloudSvc, ServiceVersionID: cloudVer},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/sdks/generate", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	hybrid.Forward(rec, req, "")

	if rec.Code != http.StatusAccepted || cloudCalled.Load() != 0 {
		t.Fatalf("expected mixed SDK to compile on Local Registry without calling Cloud, status=%d cloudCalled=%d", rec.Code, cloudCalled.Load())
	}
	if len(localReceived.InlineContracts) != 1 || localReceived.InlineContracts[0].ServiceID != cloudSvc {
		t.Fatalf("expected Cloud inline contract attached for Local Registry build, got %+v", localReceived.InlineContracts)
	}
}

// TestHybridRegistryProxyImportRoutingAndCloudOptIn verifies Local-default imports and --cloud permission + public guards.
func TestHybridRegistryProxyImportRoutingAndCloudOptIn(t *testing.T) {
	var localImports, cloudImports atomic.Int32
	localRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		localImports.Add(1)
		_, _ = w.Write([]byte(`{"plan_id":"local-plan"}`))
	}))
	defer localRegistry.Close()

	cloudRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudImports.Add(1)
		_, _ = w.Write([]byte(`{"plan_id":"cloud-plan"}`))
	}))
	defer cloudRegistry.Close()

	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{Plan: "enterprise", LocalRegistryEnabled: true})
	defer entitlement.LiveEntitlement.Reset()

	hybrid := NewHybridRegistryProxy(
		NewRegistryProxy(cloudRegistry.URL, "fk_com_ent"),
		NewRegistryProxy(localRegistry.URL, "fk_com_ent"),
		nil,
	)
	restHandler := RESTProxyHandlerWithRuntimeContracts(hybrid, nil, nil)
	workspaceID := uuid.New()

	builderWithImport := actorWithWorkspacePermissions(t, workspaceID, accesscontrol.PermissionCatalogueImport)
	adminWithCloudPublish := actorWithWorkspacePermissions(t, workspaceID, accesscontrol.PermissionCatalogueImport, accesscontrol.PermissionCatalogueCloudPublish)

	// 1. Default import plan routes strictly to Local Registry even for private specs.
	reqDefault := httptest.NewRequest(http.MethodPost, "/integrations/import/plan", bytes.NewBufferString(`{"name":"Internal","slug":"internal","is_public":false}`))
	reqDefault = reqDefault.WithContext(accesscontrol.ContextWithActor(reqDefault.Context(), builderWithImport))
	recDefault := httptest.NewRecorder()
	restHandler.ServeHTTP(recDefault, reqDefault)
	if recDefault.Code != http.StatusOK || localImports.Load() != 1 || cloudImports.Load() != 0 {
		t.Fatalf("default import misrouted: status=%d local=%d cloud=%d", recDefault.Code, localImports.Load(), cloudImports.Load())
	}

	// 2. --cloud without catalogue.cloud.publish is rejected with 403 Forbidden.
	reqDenied := httptest.NewRequest(http.MethodPost, "/integrations/import/plan", bytes.NewBufferString(`{"name":"Public","slug":"pub","is_public":true}`))
	reqDenied.Header.Set(RegistryTargetHeader, "cloud")
	reqDenied = reqDenied.WithContext(accesscontrol.ContextWithActor(reqDenied.Context(), builderWithImport))
	recDenied := httptest.NewRecorder()
	restHandler.ServeHTTP(recDenied, reqDenied)
	if recDenied.Code != http.StatusForbidden || cloudImports.Load() != 0 {
		t.Fatalf("expected 403 without catalogue.cloud.publish, got %d", recDenied.Code)
	}

	// 3. --cloud with private visibility (is_public: false) is rejected with 400 Bad Request.
	reqPrivateCloud := httptest.NewRequest(http.MethodPost, "/integrations/import/plan", bytes.NewBufferString(`{"name":"Secret","slug":"secret","is_public":false}`))
	reqPrivateCloud.Header.Set(RegistryTargetHeader, "cloud")
	reqPrivateCloud = reqPrivateCloud.WithContext(accesscontrol.ContextWithActor(reqPrivateCloud.Context(), adminWithCloudPublish))
	recPrivateCloud := httptest.NewRecorder()
	restHandler.ServeHTTP(recPrivateCloud, reqPrivateCloud)
	if recPrivateCloud.Code != http.StatusBadRequest || cloudImports.Load() != 0 {
		t.Fatalf("expected 400 for private --cloud import, got %d (%s)", recPrivateCloud.Code, recPrivateCloud.Body.String())
	}

	// 4. --cloud with catalogue.cloud.publish and is_public: true routes to Fused Cloud Registry.
	reqCloudOK := httptest.NewRequest(http.MethodPost, "/integrations/import/plan", bytes.NewBufferString(`{"name":"Public","slug":"pub","is_public":true}`))
	reqCloudOK.Header.Set(RegistryTargetHeader, "cloud")
	reqCloudOK = reqCloudOK.WithContext(accesscontrol.ContextWithActor(reqCloudOK.Context(), adminWithCloudPublish))
	recCloudOK := httptest.NewRecorder()
	restHandler.ServeHTTP(recCloudOK, reqCloudOK)
	if recCloudOK.Code != http.StatusOK || cloudImports.Load() != 1 {
		t.Fatalf("expected 200 for authorized public --cloud import, got %d (%s)", recCloudOK.Code, recCloudOK.Body.String())
	}
}

// hybridProvenanceOnlyStore implements WorkspaceServiceRegistryStore without InlineSDKContractStore to test fail-closed behavior.
type hybridProvenanceOnlyStore struct {
	store.Store
	sources map[uuid.UUID]string
}

// GetWorkspaceServiceRegistrySources returns pinned service origins for REST and mixed-SDK fail-closed tests.
func (s *hybridProvenanceOnlyStore) GetWorkspaceServiceRegistrySources(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(ids))
	for _, id := range ids {
		// Copy only known service provenance mappings.
		if src, ok := s.sources[id]; ok {
			out[id] = src
		}
	}
	return out, nil
}

// SetWorkspaceServiceRegistrySource satisfies WorkspaceServiceRegistryStore in unit tests.
func (s *hybridProvenanceOnlyStore) SetWorkspaceServiceRegistrySource(_ context.Context, serviceID uuid.UUID, source string) error {
	s.sources[serviceID] = source
	return nil
}

// TestHybridRegistryProxyPaginatedCatalogDataFallback verifies empty services.data pages fall back to Cloud when permitted.
func TestHybridRegistryProxyPaginatedCatalogDataFallback(t *testing.T) {
	var cloudCalls atomic.Int32
	localRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"services":{"data":[],"total":0,"page":1,"limit":10}}}`))
	}))
	defer localRegistry.Close()

	cloudRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudCalls.Add(1)
		_, _ = w.Write([]byte(`{"data":{"services":{"data":[{"id":"cloud-1","name":"Stripe"}],"total":1,"page":1,"limit":10}}}`))
	}))
	defer cloudRegistry.Close()

	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{Plan: "enterprise", LocalRegistryEnabled: true})
	defer entitlement.LiveEntitlement.Reset()

	hybrid := NewHybridRegistryProxy(NewRegistryProxy(cloudRegistry.URL, "fk_com_ent"), NewRegistryProxy(localRegistry.URL, "fk_com_ent"), nil)
	handler := GraphQLProxyHandler(hybrid, nil)
	wsID := uuid.New()
	admin := actorWithWorkspacePermissions(t, wsID, accesscontrol.PermissionCatalogueRead, accesscontrol.PermissionCatalogueCloudRead)
	builder := actorWithWorkspacePermissions(t, wsID, accesscontrol.PermissionCatalogueRead)

	// Authorized actor falls back to Cloud when Local Registry's paged services.data is empty.
	resAdmin := doHybridGraphQLProxyRequest(t, handler, admin, `query { services(page: 1, limit: 10, publicOnly: true) { data { id name } total } }`)
	if resAdmin.Code != http.StatusOK || resAdmin.Header().Get(RegistrySourceHeader) != sandbox.RegistrySourceCloud || cloudCalls.Load() != 1 {
		t.Fatalf("expected Cloud fallback on empty services.data, status=%d source=%q cloudCalls=%d", resAdmin.Code, resAdmin.Header().Get(RegistrySourceHeader), cloudCalls.Load())
	}

	// Builder without catalogue.cloud.read stays on Local Registry's empty page without touching Cloud.
	resBuilder := doHybridGraphQLProxyRequest(t, handler, builder, `query { services(page: 1, limit: 10, publicOnly: true) { data { id name } total } }`)
	if resBuilder.Code != http.StatusOK || cloudCalls.Load() != 1 || bytes.Contains(resBuilder.Body.Bytes(), []byte("Stripe")) {
		t.Fatalf("builder unexpectedly reached Cloud paged catalog: status=%d cloudCalls=%d body=%s", resBuilder.Code, cloudCalls.Load(), resBuilder.Body.String())
	}
}

// TestHybridRegistryProxyCloudPinnedMutationRequiresPublish verifies Cloud service mutations require catalogue.cloud.publish.
func TestHybridRegistryProxyCloudPinnedMutationRequiresPublish(t *testing.T) {
	cloudSvcID := uuid.New()
	var cloudCalls atomic.Int32
	localRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer localRegistry.Close()

	cloudRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cloudCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer cloudRegistry.Close()

	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{Plan: "enterprise", LocalRegistryEnabled: true})
	defer entitlement.LiveEntitlement.Reset()

	provStore := &hybridProvenanceOnlyStore{sources: map[uuid.UUID]string{cloudSvcID: sandbox.RegistrySourceCloud}}
	hybrid := NewHybridRegistryProxy(NewRegistryProxy(cloudRegistry.URL, "fk_com_ent"), NewRegistryProxy(localRegistry.URL, "fk_com_ent"), provStore)
	wsID := uuid.New()
	reader := actorWithWorkspacePermissions(t, wsID, accesscontrol.PermissionCatalogueCloudRead)
	publisher := actorWithWorkspacePermissions(t, wsID, accesscontrol.PermissionCatalogueCloudRead, accesscontrol.PermissionCatalogueCloudPublish)

	// Read-only GET on a Cloud-pinned service succeeds with catalogue.cloud.read.
	getReq := httptest.NewRequest(http.MethodGet, "/integrations/"+cloudSvcID.String()+"/lifecycle", nil)
	getReq = getReq.WithContext(accesscontrol.ContextWithActor(getReq.Context(), reader))
	getRec := httptest.NewRecorder()
	hybrid.Forward(getRec, getReq, "")
	if getRec.Code != http.StatusOK || cloudCalls.Load() != 1 {
		t.Fatalf("expected Cloud GET to succeed with catalogue.cloud.read, status=%d cloudCalls=%d", getRec.Code, cloudCalls.Load())
	}

	// Mutating PATCH on a Cloud-pinned service must fail with 403 when actor only has catalogue.cloud.read.
	patchDenied := httptest.NewRequest(http.MethodPatch, "/integrations/"+cloudSvcID.String()+"/details", strings.NewReader(`{"name":"X"}`))
	patchDenied = patchDenied.WithContext(accesscontrol.ContextWithActor(patchDenied.Context(), reader))
	recDenied := httptest.NewRecorder()
	hybrid.Forward(recDenied, patchDenied, "")
	if recDenied.Code != http.StatusForbidden || cloudCalls.Load() != 1 {
		t.Fatalf("expected 403 on Cloud mutation without catalogue.cloud.publish, status=%d cloudCalls=%d", recDenied.Code, cloudCalls.Load())
	}

	// Mutating PATCH succeeds once actor holds catalogue.cloud.publish.
	patchOK := httptest.NewRequest(http.MethodPatch, "/integrations/"+cloudSvcID.String()+"/details", strings.NewReader(`{"name":"X"}`))
	patchOK = patchOK.WithContext(accesscontrol.ContextWithActor(patchOK.Context(), publisher))
	recOK := httptest.NewRecorder()
	hybrid.Forward(recOK, patchOK, "")
	if recOK.Code != http.StatusOK || cloudCalls.Load() != 2 {
		t.Fatalf("expected 200 on Cloud mutation with catalogue.cloud.publish, status=%d cloudCalls=%d", recOK.Code, cloudCalls.Load())
	}
}

// TestHybridRegistryProxyStripsUntrustedInlineContracts verifies caller inline_contracts are stripped and missing snapshot stores fail closed.
func TestHybridRegistryProxyStripsUntrustedInlineContracts(t *testing.T) {
	localSvcID, cloudSvcID := uuid.New(), uuid.New()
	var localReceived models.SDKGenerationRequest
	localRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&localReceived)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"job-local","status":"pending"}`))
	}))
	defer localRegistry.Close()

	cloudRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer cloudRegistry.Close()

	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{Plan: "enterprise", LocalRegistryEnabled: true})
	defer entitlement.LiveEntitlement.Reset()

	provStore := &hybridProvenanceOnlyStore{sources: map[uuid.UUID]string{
		localSvcID: sandbox.RegistrySourceLocal,
		cloudSvcID: sandbox.RegistrySourceCloud,
	}}
	hybrid := NewHybridRegistryProxy(NewRegistryProxy(cloudRegistry.URL, "fk_com_ent"), NewRegistryProxy(localRegistry.URL, "fk_com_ent"), provStore)

	// 1. Caller-supplied inline_contracts on a Local-only SDK build must be stripped before reaching Local Registry.
	localPayload, _ := json.Marshal(models.SDKGenerationRequest{
		Name:            "local-sdk",
		Selections:      []models.SDKSelection{{ServiceID: localSvcID, ServiceVersionID: uuid.New()}},
		InlineContracts: []models.InlineSDKServiceContract{{ServiceID: localSvcID, ServiceVersionID: uuid.New(), Name: "Spoofed"}},
	})
	recLocal := httptest.NewRecorder()
	hybrid.Forward(recLocal, httptest.NewRequest(http.MethodPost, "/sdks/generate", bytes.NewReader(localPayload)), "")
	if recLocal.Code != http.StatusAccepted || len(localReceived.InlineContracts) != 0 {
		t.Fatalf("expected caller inline_contracts stripped on Local build, status=%d inlineLen=%d", recLocal.Code, len(localReceived.InlineContracts))
	}

	// 2. Mixed Local+Cloud SDK build without InlineSDKContractStore must fail closed with 409 rather than forwarding empty bytes.
	mixedPayload, _ := json.Marshal(models.SDKGenerationRequest{
		Name: "mixed-sdk",
		Selections: []models.SDKSelection{
			{ServiceID: localSvcID, ServiceVersionID: uuid.New()},
			{ServiceID: cloudSvcID, ServiceVersionID: uuid.New()},
		},
	})
	recMixed := httptest.NewRecorder()
	hybrid.Forward(recMixed, httptest.NewRequest(http.MethodPost, "/sdks/generate", bytes.NewReader(mixedPayload)), "")
	if recMixed.Code != http.StatusConflict {
		t.Fatalf("expected 409 when InlineSDKContractStore is absent for mixed SDK, got %d (%s)", recMixed.Code, recMixed.Body.String())
	}
}
