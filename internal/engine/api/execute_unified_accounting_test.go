package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/executionevent"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

type unifiedAccountingRuntime struct {
	appID       uuid.UUID
	providerURL string
	cache       *unifiedAccountingCache
	dispatcher  *engine.Dispatcher
	auth        *unifiedConnectedAuthFixture
}

// ResolveExactPhysicalOperations builds the immutable physical fixture and
// enters the production exact-operation resolver in one batch.
func (runtime *unifiedAccountingRuntime) ResolveExactPhysicalOperations(ctx context.Context, appID uuid.UUID, bindings []sandbox.ExactOperationBinding) ([]sandbox.ResolvedPhysicalOperation, error) {
	runtime.cache = newUnifiedAccountingCache(runtime.appID, runtime.providerURL, bindings, runtime.auth)
	return sandbox.ResolveExactPhysicalOperations(ctx, runtime.cache, appID, bindings)
}

// ExecuteResolvedPhysicalJSON records one scripted physical call while preserving physical execution accounting assertions.
func (runtime *unifiedAccountingRuntime) ExecuteResolvedPhysicalJSON(ctx context.Context, identity auth.RuntimeIdentity, operation sandbox.ResolvedPhysicalOperation, request sandbox.PhysicalExecutionRequest) (sandbox.PhysicalExecutionResult, error) {
	// The synthetic endpoints declare no request contract; mapped-input behavior
	// is covered by the handler tests, while this fixture isolates accounting.
	request.Params = nil
	return sandbox.ExecuteResolvedPhysicalJSON(ctx, runtime.dispatcher, identity, operation, request)
}

// ExecuteResolvedPhysicalSuccess runs an accounting-visible bodyless rollback
// through the same physical boundary used in production.
func (runtime *unifiedAccountingRuntime) ExecuteResolvedPhysicalSuccess(ctx context.Context, identity auth.RuntimeIdentity, operation sandbox.ResolvedPhysicalOperation, request sandbox.PhysicalExecutionRequest) error {
	request.Params = nil
	return sandbox.ExecuteResolvedPhysicalSuccess(ctx, runtime.dispatcher, identity, operation, request)
}

// ValidateResolvedPhysicalSelectors exercises selector admission through the production physical execution accounting interface.
func (*unifiedAccountingRuntime) ValidateResolvedPhysicalSelectors(operation sandbox.ResolvedPhysicalOperation, selectors sandbox.PhysicalExecutionSelectors) error {
	return operation.ValidateSelectors(selectors)
}

type unifiedAccountingCache struct {
	appID      uuid.UUID
	selections []byte
	services   map[string]*fusedobject.ServiceMetadata
	endpoints  map[string]fusedobject.Endpoint
}

// unifiedConnectedAuthFixture describes the immutable auth route exposed by
// the synthetic service snapshot used in resolver-to-provider integration.
type unifiedConnectedAuthFixture struct {
	authType         string
	authName         string
	configType       string
	resourceRequired bool
}

// newUnifiedAccountingCache creates one exact runtime snapshot for every
// requested physical binding without any later provider-side discovery.
func newUnifiedAccountingCache(appID uuid.UUID, providerURL string, bindings []sandbox.ExactOperationBinding, authFixture *unifiedConnectedAuthFixture) *unifiedAccountingCache {
	selections := make([]models.SDKSelection, len(bindings))
	services := make(map[string]*fusedobject.ServiceMetadata, len(bindings))
	endpoints := make(map[string]fusedobject.Endpoint, len(bindings))
	for index, binding := range bindings {
		selections[index] = models.SDKSelection{
			ServiceID: binding.ServiceID, ServiceVersionID: binding.ServiceVersionID,
			SchemaVersion: models.AppSelectionSchemaVersion, SelectAll: true,
		}
		service := &fusedobject.ServiceMetadata{
			ID: binding.ServiceID, ServiceVersionID: binding.ServiceVersionID, BaseURL: providerURL,
			ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		}
		requirements := authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}
		if authFixture != nil {
			// the selector, resolver, and dispatcher must all see the same
			// immutable auth identity compiled into the app snapshot.
			selections[index].AuthType = authFixture.authType
			selections[index].AuthName = authFixture.authName
			service.AuthConfigs = fusedobject.AuthConfigs{{Name: authFixture.authName, Type: authFixture.configType}}
			requirements = authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: authFixture.authName}}}}
			if authFixture.resourceRequired {
				service.ConnectConfig = &fusedobject.ServiceConnectConfig{AuthType: authFixture.authType, AuthName: authFixture.authName}
			}
		}
		services[binding.ServiceID.String()] = service
		path := "/github"
		if binding.EndpointName != "createIssue" {
			path = "/crm"
		}
		endpoints[binding.EndpointID.String()] = fusedobject.Endpoint{
			ID: binding.EndpointID, Name: binding.EndpointName, Method: http.MethodGet, Path: path, NormalizedPath: path,
			SecurityRequirements: requirements,
			Responses: fusedobject.Responses{"200": {
				Representations: []fusedobject.ResponseRepresentation{{
					MediaType: "application/json", Schema: &fusedobject.SchemaContract{Projection: fusedobject.Schema{Type: "object"}},
				}},
			}},
		}
	}
	encoded, _ := json.Marshal(selections)
	return &unifiedAccountingCache{appID: appID, selections: encoded, services: services, endpoints: endpoints}
}

// ConnectSDK satisfies the cache lifecycle seam; this fixture has no mutable connection state.
func (*unifiedAccountingCache) ConnectSDK(context.Context, string) error { return nil }

// DisconnectSDK satisfies the cache lifecycle seam; this fixture has no mutable connection state.
func (*unifiedAccountingCache) DisconnectSDK(string) {}

// Invalidate satisfies the cache lifecycle seam; this fixture has no mutable connection state.
func (*unifiedAccountingCache) Invalidate(string) {}

// InvalidateAppRuntime satisfies the cache lifecycle seam; this fixture has no mutable connection state.
func (*unifiedAccountingCache) InvalidateAppRuntime(string) {}

// GetOrFetchServiceMetadata returns exact fixture data through the production physical execution accounting interface.
func (cache *unifiedAccountingCache) GetOrFetchServiceMetadata(_ context.Context, appID, serviceID string) (*fusedobject.ServiceMetadata, error) {
	if appID != cache.appID.String() {
		return nil, errors.New("scope not found")
	}
	return cache.services[serviceID], nil
}

// GetEndpoint returns exact fixture data through the production physical execution accounting interface.
func (cache *unifiedAccountingCache) GetEndpoint(_ context.Context, _, _, endpointName string) (*fusedobject.Endpoint, error) {
	for _, endpoint := range cache.endpoints {
		if endpoint.Name == endpointName {
			copy := endpoint
			return &copy, nil
		}
	}
	return nil, errors.New("endpoint not found")
}

// GetAppRuntime returns exact fixture data through the production physical execution accounting interface.
func (cache *unifiedAccountingCache) GetAppRuntime(_ context.Context, appID string) (string, []byte, error) {
	if appID != cache.appID.String() {
		return "", nil, errors.New("scope not found")
	}
	return appID, append([]byte(nil), cache.selections...), nil
}

// ListExactBindingEndpoints returns exact fixture data through the production physical execution accounting interface.
func (cache *unifiedAccountingCache) ListExactBindingEndpoints(_ context.Context, _ []models.SDKSelection, bindings []sandbox.ExactOperationBinding) (map[int]fusedobject.Endpoint, error) {
	resolved := make(map[int]fusedobject.Endpoint, len(bindings))
	for index, binding := range bindings {
		resolved[index] = cache.endpoints[binding.EndpointID.String()]
	}
	return resolved, nil
}

// unifiedConnectedAuthStore supplies the exact app, connection, and resource
// rows the production SecretResolver reads while leaving unrelated Store calls
// unavailable by design.
type unifiedConnectedAuthStore struct {
	store.Store
	appRuntime      *store.AppRuntime
	connection      *store.AuthConnection
	resource        *store.ConnectionResource
	resourceCount   int
	touched         uuid.UUID
	bindingLookups  int
	connectionReads int
}

// GetAppRuntime returns the immutable bucket attached to the executing app.
func (fixture *unifiedConnectedAuthStore) GetAppRuntime(_ context.Context, appID uuid.UUID) (*store.AppRuntime, error) {
	if fixture.appRuntime == nil || fixture.appRuntime.AppID != appID {
		return nil, errors.New("app runtime not found")
	}
	copy := *fixture.appRuntime
	return &copy, nil
}

// ListWorkspaceBindingsForExecution proves the resolver uses the exact
// service/version/operation query without needing fixture-specific bindings.
func (fixture *unifiedConnectedAuthStore) ListWorkspaceBindingsForExecution(_ context.Context, _, _, _ uuid.UUID, _, _ string) ([]store.WorkspaceConnectionBinding, error) {
	fixture.bindingLookups++
	return nil, nil
}

// GetAuthConnection resolves only the exact bucket-owned connection identity.
func (fixture *unifiedConnectedAuthStore) GetAuthConnection(_ context.Context, bucketID, serviceID uuid.UUID, endUserRef, authName string) (*store.AuthConnection, error) {
	fixture.connectionReads++
	if fixture.connection == nil {
		return nil, nil
	}
	connection := fixture.connection
	if connection.BucketID != bucketID || connection.ServiceID != serviceID || connection.EndUserRef != endUserRef || connection.AuthName != authName {
		return nil, nil
	}
	copy := *connection
	return &copy, nil
}

// TouchAuthConnectionLastUsed records successful use after token decryption.
func (fixture *unifiedConnectedAuthStore) TouchAuthConnectionLastUsed(_ context.Context, id uuid.UUID, _ time.Time) error {
	fixture.touched = id
	return nil
}

// GetConnectionResourceForExecution returns the exact resource-selection
// outcome used to exercise ambiguous connected tenants.
func (fixture *unifiedConnectedAuthStore) GetConnectionResourceForExecution(_ context.Context, _ uuid.UUID, _ *uuid.UUID) (*store.ConnectionResource, int, error) {
	return fixture.resource, fixture.resourceCount, nil
}

// unifiedConnectedAuthCase captures one real resolver outcome without placing
// provider data or secrets in the public result assertions.
type unifiedConnectedAuthCase struct {
	name             string
	authType         string
	configType       string
	connectionState  string
	resourceRequired bool
	resourceCount    int
	wantStatus       string
	wantCode         string
	wantAction       string
	wantProviderCall int32
}

// TestExecuteUnifiedConnectedAuthResolverProviderIntegration exercises Unified
// through the real selector, SecretResolver, physical boundary, and dispatcher
// for OAuth/OIDC success plus every actionable pre-provider decision.
func TestExecuteUnifiedConnectedAuthResolverProviderIntegration(t *testing.T) {
	tests := []unifiedConnectedAuthCase{
		{name: "oauth success", authType: "oauth", configType: "oauth2", wantStatus: "success", wantProviderCall: 1},
		{name: "oidc success", authType: "oidc", configType: "openIdConnect", wantStatus: "success", wantProviderCall: 1},
		{name: "missing connection", authType: "oauth", configType: "oauth2", connectionState: "missing", wantStatus: "error", wantCode: "connection_required", wantAction: "connect"},
		{name: "reconnect required", authType: "oauth", configType: "oauth2", connectionState: "reconnect_required", wantStatus: "error", wantCode: "reconnect_required", wantAction: "reconnect"},
		{name: "resource selection required", authType: "oauth", configType: "oauth2", resourceRequired: true, resourceCount: 2, wantStatus: "error", wantCode: "resource_selection_required", wantAction: "select_resource"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertUnifiedConnectedAuthCase(t, test)
		})
	}
}

// assertUnifiedConnectedAuthCase runs one complete wrapper call and verifies
// whether credential resolution admitted a provider request.
func assertUnifiedConnectedAuthCase(t *testing.T, test unifiedConnectedAuthCase) {
	t.Helper()
	var providerCalls atomic.Int32
	provider := httptest.NewServer(unifiedConnectedAuthProvider(t, &providerCalls))
	defer provider.Close()

	server, _, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
	authFixture := &unifiedConnectedAuthFixture{
		authType: test.authType, authName: "connectedAuth", configType: test.configType,
		resourceRequired: test.resourceRequired,
	}
	server.unifiedRuntime = &unifiedAccountingRuntime{
		appID: appID, providerURL: provider.URL, dispatcher: engine.NewDispatcher(), auth: authFixture,
	}
	connectedStore := newUnifiedConnectedAuthStore(t, appID, test)
	previousResolver := sandbox.SetSecretResolver(sandbox.NewSecretResolver(connectedStore, unifiedConnectedAuthMasterKey))
	t.Cleanup(func() { sandbox.SetSecretResolver(previousResolver) })
	installUnifiedAccountingCaptures(t)

	request := unifiedRuntimeRequest()
	request.Targets = []string{"github"}
	request.TargetSelectors = map[string]*enginev1.ExecutionSelectors{
		"github": {EndUserRef: "connected-user", AuthType: test.authType, AuthName: "connectedAuth"},
	}
	response, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if err != nil || len(response.GetResults()) != 1 {
		t.Fatalf("ExecuteUnified() = (%#v, %v)", response, err)
	}
	assertUnifiedConnectedAuthResolution(t, response.GetResults()[0], test, providerCalls.Load(), connectedStore)
}

// unifiedConnectedAuthProvider hosts the real HTTP boundary and rejects any
// request that did not receive the decrypted connected bearer token.
func unifiedConnectedAuthProvider(t *testing.T, providerCalls *atomic.Int32) http.HandlerFunc {
	t.Helper()
	return func(response http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		if request.Header.Get("Authorization") != "Bearer connected-token" {
			t.Errorf("provider Authorization = %q", request.Header.Get("Authorization"))
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"id":"provider-1"}`))
	}
}

// assertUnifiedConnectedAuthResolution correlates provider admission, public
// remediation metadata, resolver query counts, and last-used auditing.
func assertUnifiedConnectedAuthResolution(t *testing.T, result *enginev1.UnifiedTargetResult, test unifiedConnectedAuthCase, providerCalls int32, connectedStore *unifiedConnectedAuthStore) {
	t.Helper()
	if result.GetStatus() != test.wantStatus || result.GetErrorCode() != test.wantCode || providerCalls != test.wantProviderCall {
		t.Fatalf("result/provider calls = (%#v, %d), want status:%s code:%s calls:%d", result, providerCalls, test.wantStatus, test.wantCode, test.wantProviderCall)
	}
	assertUnifiedConnectedAuthAction(t, result.GetAuthAction(), test.wantAction)
	if connectedStore.bindingLookups != 1 || connectedStore.connectionReads != 1 {
		t.Fatalf("resolver queries = bindings:%d connections:%d, want one/one", connectedStore.bindingLookups, connectedStore.connectionReads)
	}
	if test.wantStatus == "success" && connectedStore.touched == uuid.Nil {
		t.Fatal("successful connected auth did not update last-used audit state")
	}
}

var unifiedConnectedAuthMasterKey = []byte("12345678901234567890123456789012")

// newUnifiedConnectedAuthStore encrypts one fixture token with the same DEK
// envelope the production resolver decrypts immediately before dispatch.
func newUnifiedConnectedAuthStore(t *testing.T, appID uuid.UUID, test unifiedConnectedAuthCase) *unifiedConnectedAuthStore {
	t.Helper()
	bucketID := uuid.New()
	serviceID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	selections, _ := json.Marshal([]models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: uuid.New(), SchemaVersion: models.AppSelectionSchemaVersion,
	}})
	fixture := &unifiedConnectedAuthStore{
		appRuntime: &store.AppRuntime{
			AppID: appID, BucketID: bucketID, ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: selections,
		},
		resourceCount: test.resourceCount,
	}
	if test.connectionState == "missing" {
		return fixture
	}
	wrapper, dek, err := store.WrapDEK(unifiedConnectedAuthMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	accessToken, err := store.EncryptWithDEK(dek, "connected-token")
	if err != nil {
		t.Fatal(err)
	}
	fixture.connection = &store.AuthConnection{
		ID: uuid.New(), BucketID: bucketID, ServiceID: serviceID,
		EndUserRef: "connected-user", AuthType: test.authType, AuthName: "connectedAuth",
		EncryptedDEK: wrapper, EncryptedAccessToken: accessToken, RefreshState: test.connectionState,
	}
	return fixture
}

// assertUnifiedConnectedAuthAction verifies bounded remediation metadata and
// its absence on successful provider calls.
func assertUnifiedConnectedAuthAction(t *testing.T, action *enginev1.UnifiedAuthAction, want string) {
	t.Helper()
	if want == "" {
		if action != nil {
			t.Fatalf("unexpected auth action = %#v", action)
		}
		return
	}
	if action == nil || action.GetAction() != want || action.GetBucketId() == "" || action.GetServiceId() == "" || action.GetEndUserRef() != "connected-user" {
		t.Fatalf("auth action = %#v, want %s with bounded routing metadata", action, want)
	}
}

type unifiedEventCapture struct {
	mu       sync.Mutex
	messages [][]byte
}

// PublishMsgJS captures finalized execution receipts through the production event interface.
func (capture *unifiedEventCapture) PublishMsgJS(message *nats.Msg) (*nats.PubAck, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.messages = append(capture.messages, append([]byte(nil), message.Data...))
	return &nats.PubAck{}, nil
}

type unifiedUsageCapture struct {
	mu         sync.Mutex
	increments []models.EngineUsageIncrement
}

// Record captures usage increments so tests can detect logical-wrapper double counting.
func (capture *unifiedUsageCapture) Record(increment models.EngineUsageIncrement) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.increments = append(capture.increments, increment)
}

// TestExecuteUnifiedProducesOnlyPhysicalAccounting protects the rule that each physical attempt produces one receipt and the matching usage outcome.
func TestExecuteUnifiedProducesOnlyPhysicalAccounting(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if strings.Contains(request.URL.Path, "crm") {
			_, _ = response.Write([]byte(`{"iid":"crm-1"}`))
			return
		}
		_, _ = response.Write([]byte(`{"id":"gh-1"}`))
	}))
	defer provider.Close()

	server, _, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
	server.unifiedRuntime = &unifiedAccountingRuntime{appID: appID, providerURL: provider.URL, dispatcher: engine.NewDispatcher()}
	events, usage := installUnifiedAccountingCaptures(t)

	request := unifiedRuntimeRequest()
	request.TargetSelectors = nil
	response, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if err != nil || len(response.GetResults()) != 2 {
		t.Fatalf("ExecuteUnified() = (%#v, %v)", response, err)
	}
	assertUnifiedResults(t, response, []string{"github", "@acme/custom-crm"}, []string{`{"id":"gh-1"}`, `{"iid":"crm-1"}`})
	if len(events.messages) != 2 {
		t.Fatalf("execution events = %d, want two physical events and no wrapper event", len(events.messages))
	}
	assertUnifiedPhysicalEvents(t, events.messages, appID)
	assertUnifiedUsageIncrements(t, usage.increments, 2)
}

// TestExecuteUnifiedSingleTargetProducesOnePhysicalAccountingFinalization locks
// the single-target Engine/usage path requested for the outcome matrix.
func TestExecuteUnifiedSingleTargetProducesOnePhysicalAccountingFinalization(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"id":"gh-1"}`))
	}))
	defer provider.Close()

	server, _, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
	server.unifiedRuntime = &unifiedAccountingRuntime{appID: appID, providerURL: provider.URL, dispatcher: engine.NewDispatcher()}
	events, usage := installUnifiedAccountingCaptures(t)
	request := unifiedRuntimeRequest()
	request.Targets, request.TargetSelectors = []string{"github"}, nil
	response, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if err != nil || len(response.GetResults()) != 1 || response.GetResults()[0].GetStatus() != "success" {
		t.Fatalf("ExecuteUnified() = (%#v, %v)", response, err)
	}
	if len(events.messages) != 1 {
		t.Fatalf("execution events = %d, want one physical event", len(events.messages))
	}
	assertUnifiedUsageOutcomes(t, usage.increments, 1, 1, 0)
}

// TestExecuteUnifiedRealMixedFailureAttemptsBothPhysicalTargets proves a real
// provider 500 is isolated after both independent calls enter the boundary.
func TestExecuteUnifiedRealMixedFailureAttemptsBothPhysicalTargets(t *testing.T) {
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		response.Header().Set("Content-Type", "application/json")
		if strings.Contains(request.URL.Path, "crm") {
			response.WriteHeader(http.StatusInternalServerError)
			_, _ = response.Write([]byte(`{"error":"fixture"}`))
			return
		}
		_, _ = response.Write([]byte(`{"id":"gh-1"}`))
	}))
	defer provider.Close()

	server, _, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
	server.unifiedRuntime = &unifiedAccountingRuntime{appID: appID, providerURL: provider.URL, dispatcher: engine.NewDispatcher()}
	events, usage := installUnifiedAccountingCaptures(t)
	request := unifiedRuntimeRequest()
	request.TargetSelectors = nil
	response, err := server.ExecuteUnified(grpcTestContext(appID), request)
	if err != nil || len(response.GetResults()) != 2 {
		t.Fatalf("ExecuteUnified() = (%#v, %v)", response, err)
	}
	if response.GetResults()[0].GetStatus() != "success" || response.GetResults()[1].GetErrorCode() != "provider_error" {
		t.Fatalf("mixed response = %#v", response.GetResults())
	}
	if providerCalls.Load() != 2 || len(events.messages) != 2 {
		t.Fatalf("physical attempts = provider:%d events:%d, want two/two", providerCalls.Load(), len(events.messages))
	}
	assertUnifiedUsageOutcomes(t, usage.increments, 2, 1, 1)
}

// TestExecuteUnifiedPredispatchRejectionProducesNoAccounting protects the rule that each physical attempt produces one receipt and the matching usage outcome.
func TestExecuteUnifiedPredispatchRejectionProducesNoAccounting(t *testing.T) {
	server, _, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowedOperations: []string{"createIssue"}})
	server.unifiedRuntime = &unifiedAccountingRuntime{appID: appID, dispatcher: engine.NewDispatcher()}
	events, usage := installUnifiedAccountingCaptures(t)

	request := unifiedRuntimeRequest()
	request.TargetSelectors = nil
	if _, err := server.ExecuteUnified(grpcTestContext(appID), request); err == nil {
		t.Fatal("ExecuteUnified() accepted an unauthorized target")
	}
	if len(events.messages) != 0 || len(usage.increments) != 0 {
		t.Fatalf("predispatch accounting = events:%d usage:%d", len(events.messages), len(usage.increments))
	}
}

// installUnifiedAccountingCaptures swaps in receipt and usage recorders and
// restores both globals after the current test.
func installUnifiedAccountingCaptures(t *testing.T) (*unifiedEventCapture, *unifiedUsageCapture) {
	t.Helper()
	events := &unifiedEventCapture{}
	usage := &unifiedUsageCapture{}
	executionevent.SetPublisher(executionevent.NewPublisher(events))
	sandbox.SetExecutionUsageRecorder(usage)
	previousEntitlement := entitlement.LiveEntitlement.Load()
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{MaxSandboxConcurrency: models.IntPtr(8)})
	t.Cleanup(func() {
		executionevent.SetPublisher(nil)
		sandbox.SetExecutionUsageRecorder(nil)
		entitlement.LiveEntitlement.Store(previousEntitlement)
	})
	return events, usage
}

// assertUnifiedPhysicalEvents requires one finalized SDK execution receipt per
// provider attempt and rejects any logical-wrapper receipt.
func assertUnifiedPhysicalEvents(t *testing.T, messages [][]byte, appID uuid.UUID) {
	t.Helper()
	for _, message := range messages {
		var envelope models.EngineExecutionEventEnvelope
		if err := json.Unmarshal(message, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Event.AppID != appID || envelope.Event.OperationID == uuid.Nil || envelope.Event.ServiceID == uuid.Nil {
			t.Fatalf("event is not a physical execution: %#v", envelope.Event)
		}
	}
}

// assertUnifiedUsageIncrements checks that every successful physical child
// contributes exactly one total and one success usage increment.
func assertUnifiedUsageIncrements(t *testing.T, increments []models.EngineUsageIncrement, physicalCount int) {
	t.Helper()
	assertUnifiedUsageOutcomes(t, increments, physicalCount, physicalCount, 0)
}

// assertUnifiedUsageOutcomes verifies one total and one terminal metric per
// physical execution without introducing a logical-wrapper usage record.
func assertUnifiedUsageOutcomes(t *testing.T, increments []models.EngineUsageIncrement, total, succeeded, failed int) {
	t.Helper()
	got := make(map[string]int)
	for _, increment := range increments {
		got[increment.Metric] += int(increment.Count)
	}
	if len(increments) != total*2 || got[models.EngineUsageMetricExecutionTotal] != total || got[models.EngineUsageMetricExecutionSuccess] != succeeded || got[models.EngineUsageMetricExecutionFailed] != failed {
		t.Fatalf("usage increments = %#v, want total:%d success:%d failed:%d", increments, total, succeeded, failed)
	}
}

var _ unifiedPhysicalRuntime = (*unifiedAccountingRuntime)(nil)
var _ sandbox.ObjectCache = (*unifiedAccountingCache)(nil)
