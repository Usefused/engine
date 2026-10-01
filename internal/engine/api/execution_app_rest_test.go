package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type capabilityRouteStore struct {
	store.Store
	mu           sync.Mutex
	bundle       store.UnifiedAppBundle
	activeAppID  uuid.UUID
	familyID     uuid.UUID
	server       *EngineGRPCServer
	record       store.ExecutionResult
	records      map[uuid.UUID]store.ExecutionResult
	startEntered chan struct{}
	startRelease chan struct{}
}

type blockingCapabilityRuntime struct {
	*restRuntimeTestDouble
	entered chan struct{}
	release chan struct{}
}

// TestUnifiedAppRejectsUnboundManifest keeps authored code inside a selected workspace operation scope.
func TestUnifiedAppRejectsUnboundManifest(t *testing.T) {
	_, err := parseUnifiedAppManifest(json.RawMessage(`{"schemaVersion":1,"inputSchema":{"type":"object"},"outputSchema":{"type":"object"},"searchable":[],"selectedOperations":[]}`))
	// A valid schema alone cannot establish an executable app version.
	if err == nil {
		t.Fatal("unbound unified app manifest was admitted")
	}
}

// TestUnifiedAppKeepsSelectedRawRESTOperation proves authored execute does not replace physical access.
func TestUnifiedAppKeepsSelectedRawRESTOperation(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true}
	_, fixture, familyID := newCapabilityRouteFixture(runtime, nil)
	response := performRESTExecution(t, fixture.server, familyID, "fsk_test", `{"operation":"issues.get","input":{"id":7}}`, "")
	// Selected physical calls still use the Engine dispatcher and its request-scoped cache.
	if response.Code != http.StatusOK || runtime.connects != 1 || runtime.resolvedOperation != "issues.get" {
		t.Fatalf("raw execution status=%d connects=%d operation=%q body=%s", response.Code, runtime.connects, runtime.resolvedOperation, response.Body.String())
	}
}

// TestUnifiedAppOldVersionRejectsNewTraffic prevents exact version IDs from becoming public aliases.
func TestUnifiedAppOldVersionRejectsNewTraffic(t *testing.T) {
	router, fixture, _ := newCapabilityRouteFixture(&restRuntimeTestDouble{}, nil)
	appID := fixture.bundle.AppID
	request := httptest.NewRequest(http.MethodPost, "/v1/apps/"+appID.String()+"/executions", strings.NewReader(`{"operation":"execute","input":{"value":"old"}}`))
	request.Header.Set("Authorization", "Bearer fsk_test")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	// The refusal happens before accepting a durable execution or touching the provider.
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "app_family_id_required") || len(fixture.records) != 0 {
		t.Fatalf("old version status=%d body=%s records=%d", response.Code, response.Body.String(), len(fixture.records))
	}
}

// ConnectAppRuntime holds provider setup so an asynchronous response can be inspected while queued.
func (runtime *blockingCapabilityRuntime) ConnectAppRuntime(ctx context.Context, appID uuid.UUID) error {
	close(runtime.entered)
	// The worker remains cancellation aware while the test observes its durable reservation.
	select {
	case <-runtime.release:
		return runtime.restRuntimeTestDouble.ConnectAppRuntime(ctx, appID)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newCapabilityRouteFixture installs one exact app bundle and family token for HTTP adapter tests.
func newCapabilityRouteFixture(baseRuntime *restRuntimeTestDouble, runtime restExecutionRuntime) (*chi.Mux, *capabilityRouteStore, uuid.UUID) {
	server, appID := newRESTPhysicalServer(baseRuntime)
	// A blocking test adapter can replace only cache acquisition without rebuilding the server fixture.
	if runtime != nil {
		server.restRuntime = runtime
	}
	base := server.store.(*grpcRuntimeStore)
	base.scope.Version = "1.0.0"
	base.scope.Kind = store.AppKindUnifiedApp
	identity := auth.RuntimeIdentity{
		AccountID: base.accountID, AppFamilyID: uuid.New(), AppID: appID,
		TokenID: uuid.New(), AppVersion: "1.0.0", Kind: store.AppKindUnifiedApp, Status: store.AppStatusActive,
		TokenPolicy: store.AppTokenPolicy{AllowAll: true},
	}
	server.tokenValidator = appTestValidator{identity: identity}
	manifest := `{"schemaVersion":1,"inputSchema":{"type":"object"},"outputSchema":{"type":"object"},"searchable":["value"],"selectedOperations":[{"service":"crm","operation":"createIssue","serviceId":"11111111-1111-4111-8111-111111111111","serviceVersionId":"22222222-2222-4222-8222-222222222222","endpointId":"33333333-3333-4333-8333-333333333333"}]}`
	// Resident workers inspect the bundled manifest before execution, just as production compiler output requires.
	bundle := `globalThis.FusedExecutionManifest=` + manifest + `;globalThis.FusedUnifiedApp={input:{parse(v){if(typeof v.value!=="string")throw Error("invalid");return v}},output:{parse(v){if(typeof v.value!=="string")throw Error("invalid");return v}},async execute({input}){await __fusedHost.dbSet(JSON.stringify({value:input.value}));return {value:input.value}}};`
	base.scope.AppFamilyID = identity.AppFamilyID
	fixture := &capabilityRouteStore{Store: base, server: server, familyID: identity.AppFamilyID, activeAppID: appID, bundle: store.UnifiedAppBundle{AppID: appID, SourceHash: "sha256:test", BundleJS: bundle, Manifest: json.RawMessage(manifest)}}
	server.store = fixture
	router := chi.NewRouter()
	MountAppExecutionRoute(router, server)
	MountUnifiedAppRoutes(router, server)
	MountExecutionResultRoutes(router, server)
	return router, fixture, identity.AppFamilyID
}

// IsUnifiedAppTrafficTarget models the persisted family pointer for route admission tests.
func (fixture *capabilityRouteStore) IsUnifiedAppTrafficTarget(_ context.Context, appID uuid.UUID) (bool, error) {
	return fixture.activeAppID == appID, nil
}

// GetApp binds the fixture bundle to the exact immutable app source identity.
func (fixture *capabilityRouteStore) GetApp(_ context.Context, appID uuid.UUID) (*store.App, error) {
	// Sibling IDs cannot substitute their own bundle at the execution route.
	if appID != fixture.bundle.AppID {
		return nil, store.ErrAppNotFound
	}
	return &store.App{AppID: appID, SourceHash: fixture.bundle.SourceHash, BundleDigest: store.UnifiedAppBundleDigest([]byte(fixture.bundle.BundleJS))}, nil
}

// GetUnifiedAppBundle supplies the exact script and descriptor for the authenticated app version.
func (fixture *capabilityRouteStore) GetUnifiedAppBundle(_ context.Context, appID uuid.UUID) (*store.UnifiedAppBundle, error) {
	// A sibling version cannot inherit this test bundle.
	if appID != fixture.bundle.AppID {
		return nil, store.ErrUnifiedAppBundleNotFound
	}
	return &fixture.bundle, nil
}

// CreateUnifiedAppBundle is unused because deployment is a separate control mutation.
func (*capabilityRouteStore) CreateUnifiedAppBundle(context.Context, store.UnifiedAppBundle) error {
	return errors.New("test fixture does not deploy bundles")
}

// CreateExecutionResult proves the live route records the call before running authored code.
func (fixture *capabilityRouteStore) CreateExecutionResult(_ context.Context, record store.ExecutionResult) error {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	// Distinct live and rerun calls retain independent records in this fixture.
	if fixture.records == nil {
		fixture.records = make(map[uuid.UUID]store.ExecutionResult)
	}
	fixture.record = record
	fixture.record.Data = json.RawMessage(`null`)
	fixture.record.CreatedAt = time.Now()
	fixture.records[record.ID] = fixture.record
	return nil
}

// CreateOrGetRerunExecutionResult preserves the first reservation for one caller key.
func (fixture *capabilityRouteStore) CreateOrGetRerunExecutionResult(ctx context.Context, record store.ExecutionResult) (uuid.UUID, bool, error) {
	fixture.mu.Lock()
	for _, existing := range fixture.records {
		// A duplicate key for the same source must never dispatch a second live call.
		if existing.Mode == "rerun" && existing.SourceExecutionID != nil && record.SourceExecutionID != nil &&
			*existing.SourceExecutionID == *record.SourceExecutionID && existing.IdempotencyKeyHash == record.IdempotencyKeyHash {
			fixture.mu.Unlock()
			return existing.ID, false, nil
		}
	}
	fixture.mu.Unlock()
	return record.ID, true, fixture.CreateExecutionResult(ctx, record)
}

// StartExecutionResult represents the one accepted queued-to-running transition.
func (fixture *capabilityRouteStore) StartExecutionResult(ctx context.Context, _, _, id uuid.UUID) error {
	// The asynchronous fixture can hold the queued state without requiring a provider cache for DB-only code.
	if fixture.startEntered != nil && fixture.startRelease != nil {
		close(fixture.startEntered)
		select {
		case <-fixture.startRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	record := fixture.records[id]
	// A different execution ID cannot start this fixture's record.
	if record.ID == uuid.Nil || record.Status != "queued" {
		return store.ErrExecutionResultTransition
	}
	record.Status = "running"
	fixture.records[id] = record
	fixture.record = record
	return nil
}

// CompleteExecutionResult makes output and terminal status visible together.
func (fixture *capabilityRouteStore) CompleteExecutionResult(_ context.Context, _, _, id uuid.UUID, status string, output, data json.RawMessage, code, message string) error {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	record := fixture.records[id]
	// A terminal state cannot be replaced by a later worker response.
	if record.ID == uuid.Nil || record.Status != "running" {
		return store.ErrExecutionResultTransition
	}
	record.Status, record.Output, record.Data = status, output, data
	record.ErrorCode, record.ErrorMessage = code, message
	completed := time.Now()
	record.CompletedAt = &completed
	fixture.records[id] = record
	fixture.record = record
	return nil
}

// GetExecutionResult returns the one record without granting access to sibling app identities.
func (fixture *capabilityRouteStore) GetExecutionResult(_ context.Context, _, _, id uuid.UUID) (*store.ExecutionResult, error) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	record, found := fixture.records[id]
	// Missing IDs are indistinguishable from another caller's execution.
	if !found {
		return nil, store.ErrExecutionResultNotFound
	}
	return &record, nil
}

// SearchExecutionResults is unused by the direct invocation fixture.
func (*capabilityRouteStore) SearchExecutionResults(context.Context, store.ExecutionResultSearch) ([]store.ExecutionResult, error) {
	return nil, errors.New("test fixture does not search")
}

// DeleteExpiredExecutionResults is unused because retention is tested in the store package.
func (*capabilityRouteStore) DeleteExpiredExecutionResults(context.Context, time.Time, int) (int64, error) {
	return 0, errors.New("test fixture does not sweep")
}

// TestProjectCapabilityExecutionKeepsSearchDataPrivate verifies the POST projection without requiring an isolated worker.
func TestProjectCapabilityExecutionKeepsSearchDataPrivate(t *testing.T) {
	record := &store.ExecutionResult{Output: json.RawMessage(`{"customerId":"cus_123"}`), Data: json.RawMessage(`{"customerId":"cus_123"}`)}
	encoded, err := json.Marshal(projectCapabilityExecution(record, "caller-handle"))
	if err != nil {
		t.Fatal(err)
	}
	// The caller receives output and its handle while the persisted document stays out of the POST body.
	if !strings.Contains(string(encoded), `"output":{"customerId":"cus_123"}`) || !strings.Contains(string(encoded), `"readHandle":"caller-handle"`) || strings.Contains(string(encoded), `"data":`) || string(record.Data) != `{"customerId":"cus_123"}` {
		t.Fatalf("execution response leaked stored data or lost output: %s", encoded)
	}
}

// TestCapabilityExecutionPersistsTypedOutputAndData exercises the authenticated public route and real JavaScript runner.
func TestCapabilityExecutionPersistsTypedOutputAndData(t *testing.T) {
	requireCapabilityWorkerForDarwin(t)
	runtime := &restRuntimeTestDouble{}
	router, fixture, appID := newCapabilityRouteFixture(runtime, nil)
	result := invokeCapabilityRouteForTest(t, router, fixture, appID)
	assertCapabilityResultFetch(t, router, runtime, appID, result)
	rerun := assertCapabilityRerun(t, router, runtime, appID, result)
	assertCapabilityRerunDuplicate(t, router, runtime, appID, result, rerun)
}

// TestUnifiedAppPromotionRetainsHistoricalFetch keeps completed results readable after the version stops taking traffic.
func TestUnifiedAppPromotionRetainsHistoricalFetch(t *testing.T) {
	requireCapabilityWorkerForDarwin(t)
	runtime := &restRuntimeTestDouble{}
	router, fixture, appID := newCapabilityRouteFixture(runtime, nil)
	result := invokeCapabilityRouteForTest(t, router, fixture, appID)
	promoteCapabilityFixture(fixture)
	// Promotion changes execution admission, not the caller's already-issued result handle.
	assertCapabilityResultFetch(t, router, runtime, appID, result)
	rerun := httptest.NewRequest(http.MethodPost, "/v1/apps/"+appID.String()+"/executions/"+result.ExecutionID.String()+"/rerun", nil)
	rerun.Header.Set("Authorization", "Bearer fsk_test")
	rerun.Header.Set("X-Execution-Read-Handle", result.ReadHandle)
	rerun.Header.Set("Idempotency-Key", "old-version-retry")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, rerun)
	// A retained result never grants permission to create new work on the old version.
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "app_version_not_current") {
		t.Fatalf("old version rerun status=%d body=%s", response.Code, response.Body.String())
	}
}

// TestUnifiedAppHasNoNamedCapabilityRoute keeps one app-level execute entrypoint on the SDK REST surface.
func TestUnifiedAppHasNoNamedCapabilityRoute(t *testing.T) {
	router, _, appID := newCapabilityRouteFixture(&restRuntimeTestDouble{}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/apps/"+appID.String()+"/capabilities/echo/executions", strings.NewReader(`{"input":{}}`))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	// An obsolete named path must not select authored code or create an execution record.
	if response.Code != http.StatusNotFound {
		t.Fatalf("obsolete route status = %d", response.Code)
	}
}

// invokeCapabilityRouteForTest checks the durable handle and output while keeping the stored search document out of the response.
func invokeCapabilityRouteForTest(t *testing.T, router *chi.Mux, fixture *capabilityRouteStore, appID uuid.UUID) capabilityExecutionEnvelope {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/apps/"+appID.String()+"/executions", strings.NewReader(`{"operation":"execute","input":{"value":"Jane"}}`))
	request.Header.Set("Authorization", "Bearer fsk_test")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	// The route returns the typed output while retaining the search document only in storage.
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var result capabilityExecutionEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExecutionID == uuid.Nil || len(result.ReadHandle) != 64 || result.Status != "succeeded" || string(result.Output) != `{"value":"Jane"}` || string(fixture.record.Data) != `{"value":"Jane"}` || strings.Contains(recorder.Body.String(), `"data":`) {
		t.Fatalf("execution result = %+v", result)
	}
	// Storage must hold only a digest of the caller's one-time read handle.
	if fixture.record.ReadHandleHash == result.ReadHandle || fixture.record.Status != "succeeded" {
		t.Fatal("read handle was stored raw or execution did not complete")
	}
	return result
}

// assertCapabilityResultFetch proves authorized output reads, private search data, and wrong-handle isolation without another execution.
func assertCapabilityResultFetch(t *testing.T, router *chi.Mux, runtime *restRuntimeTestDouble, appID uuid.UUID, result capabilityExecutionEnvelope) {
	t.Helper()
	read := httptest.NewRequest(http.MethodGet, "/v1/apps/"+appID.String()+"/executions/"+result.ExecutionID.String(), nil)
	read.Header.Set("Authorization", "Bearer fsk_test")
	read.Header.Set("X-Execution-Read-Handle", result.ReadHandle)
	readResponse := httptest.NewRecorder()
	router.ServeHTTP(readResponse, read)
	// Fetch by ID returns the output while keeping the stored search document private.
	// A selected app acquires the workspace operation cache even when this run only writes data.
	if readResponse.Code != http.StatusOK || !strings.Contains(readResponse.Body.String(), `"value":"Jane"`) || strings.Contains(readResponse.Body.String(), `"data":`) || runtime.connects != 1 {
		t.Fatalf("read status=%d body=%s connects=%d", readResponse.Code, readResponse.Body.String(), runtime.connects)
	}
	read.Header.Set("X-Execution-Read-Handle", strings.Repeat("0", 64))
	invalidResponse := httptest.NewRecorder()
	router.ServeHTTP(invalidResponse, read)
	// A guessed execution ID and wrong handle must not disclose durable output.
	if invalidResponse.Code != http.StatusNotFound {
		t.Fatalf("wrong-handle status=%d body=%s", invalidResponse.Code, invalidResponse.Body.String())
	}
}

// assertCapabilityRerun proves explicit live effects and idempotent duplicate reservation.
func assertCapabilityRerun(t *testing.T, router *chi.Mux, runtime *restRuntimeTestDouble, appID uuid.UUID, result capabilityExecutionEnvelope) capabilityExecutionEnvelope {
	t.Helper()
	rerunPath := "/v1/apps/" + appID.String() + "/executions/" + result.ExecutionID.String() + "/rerun"
	rerun := httptest.NewRequest(http.MethodPost, rerunPath, nil)
	rerun.Header.Set("Authorization", "Bearer fsk_test")
	rerun.Header.Set("X-Execution-Read-Handle", result.ReadHandle)
	rerun.Header.Set("Idempotency-Key", "customer-onboard-again")
	rerunResponse := httptest.NewRecorder()
	router.ServeHTTP(rerunResponse, rerun)
	var rerunResult capabilityExecutionEnvelope
	decodeRerunErr := json.Unmarshal(rerunResponse.Body.Bytes(), &rerunResult)
	// Rerun gets a new linked ID and fresh data through the same selected app version.
	if rerunResponse.Code != http.StatusOK || decodeRerunErr != nil || rerunResult.ExecutionID == result.ExecutionID ||
		rerunResult.SourceExecutionID == nil || *rerunResult.SourceExecutionID != result.ExecutionID || rerunResult.Mode != "rerun" || runtime.connects != 2 {
		t.Fatalf("rerun status=%d body=%s decode=%v connects=%d", rerunResponse.Code, rerunResponse.Body.String(), decodeRerunErr, runtime.connects)
	}
	return rerunResult
}

// assertCapabilityRerunDuplicate proves a repeated key cannot replay provider effects or a read handle.
func assertCapabilityRerunDuplicate(t *testing.T, router *chi.Mux, runtime *restRuntimeTestDouble, appID uuid.UUID, result, rerunResult capabilityExecutionEnvelope) {
	t.Helper()
	rerunPath := "/v1/apps/" + appID.String() + "/executions/" + result.ExecutionID.String() + "/rerun"
	duplicate := httptest.NewRequest(http.MethodPost, rerunPath, nil)
	duplicate.Header.Set("Authorization", "Bearer fsk_test")
	duplicate.Header.Set("X-Execution-Read-Handle", result.ReadHandle)
	duplicate.Header.Set("Idempotency-Key", "customer-onboard-again")
	duplicateResponse := httptest.NewRecorder()
	router.ServeHTTP(duplicateResponse, duplicate)
	var duplicateResult capabilityExecutionEnvelope
	decodeDuplicateErr := json.Unmarshal(duplicateResponse.Body.Bytes(), &duplicateResult)
	// The same key returns the first rerun ID without reissuing its secret handle or reacquiring provider scope.
	if duplicateResponse.Code != http.StatusOK || decodeDuplicateErr != nil || duplicateResult.ExecutionID != rerunResult.ExecutionID ||
		duplicateResult.ReadHandle != "" || runtime.connects != 2 {
		t.Fatalf("duplicate status=%d body=%s decode=%v connects=%d", duplicateResponse.Code, duplicateResponse.Body.String(), decodeDuplicateErr, runtime.connects)
	}
}

// TestCapabilityExecutionRespondAsyncReturnsPendingHandle verifies durable admission before worker effects.
func TestCapabilityExecutionRespondAsyncReturnsPendingHandle(t *testing.T) {
	runtime := &restRuntimeTestDouble{}
	router, fixture, appID := newCapabilityRouteFixture(runtime, nil)
	fixture.startEntered, fixture.startRelease = make(chan struct{}), make(chan struct{})
	// Cleanup releases the worker even if an assertion stops this test early.
	t.Cleanup(func() { closeCapabilityRelease(fixture.startRelease) })
	accepted := invokeAsyncCapabilityForTest(t, router, appID)
	waitCapabilityWorkerEntry(t, fixture.startEntered)
	assertPendingCapabilityFetch(t, router, appID, accepted)
	closeCapabilityRelease(fixture.startRelease)
	waitCapabilityTerminal(t, fixture, accepted.ExecutionID)
}

// invokeAsyncCapabilityForTest receives the one-time handle before the blocked worker starts effects.
func invokeAsyncCapabilityForTest(t *testing.T, router *chi.Mux, appID uuid.UUID) capabilityExecutionEnvelope {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/apps/"+appID.String()+"/executions", strings.NewReader(`{"operation":"execute","input":{"value":"Jane"}}`))
	request.Header.Set("Authorization", "Bearer fsk_test")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Prefer", "respond-async")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var accepted capabilityExecutionEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode pending result: %v", err)
	}
	// The response includes a one-time read handle before cache acquisition or provider work finishes.
	if response.Code != http.StatusAccepted || accepted.Status != "queued" || accepted.ExecutionID == uuid.Nil || len(accepted.ReadHandle) != 64 {
		t.Fatalf("pending response status=%d result=%+v", response.Code, accepted)
	}
	return accepted
}

// waitCapabilityWorkerEntry bounds the test's wait for detached cache acquisition.
func waitCapabilityWorkerEntry(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("detached worker did not start")
	}
}

// assertPendingCapabilityFetch proves the issued read handle works before completion.
func assertPendingCapabilityFetch(t *testing.T, router *chi.Mux, appID uuid.UUID, accepted capabilityExecutionEnvelope) {
	t.Helper()
	read := httptest.NewRequest(http.MethodGet, "/v1/apps/"+appID.String()+"/executions/"+accepted.ExecutionID.String(), nil)
	read.Header.Set("Authorization", "Bearer fsk_test")
	read.Header.Set("X-Execution-Read-Handle", accepted.ReadHandle)
	readResponse := httptest.NewRecorder()
	router.ServeHTTP(readResponse, read)
	// A pending result is fetchable by its ID while the worker waits on runtime admission.
	if readResponse.Code != http.StatusOK || !strings.Contains(readResponse.Body.String(), `"status":"queued"`) {
		t.Fatalf("pending fetch status=%d body=%s", readResponse.Code, readResponse.Body.String())
	}
}

// closeCapabilityRelease closes the worker gate once, including on test cleanup.
func closeCapabilityRelease(release chan struct{}) {
	select {
	case <-release:
	default:
		close(release)
	}
}

// waitCapabilityTerminal waits for a result while accepting either supported worker outcome.
func waitCapabilityTerminal(t *testing.T, fixture *capabilityRouteStore, executionID uuid.UUID) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		fixture.mu.Lock()
		status := fixture.records[executionID].Status
		fixture.mu.Unlock()
		// The isolated worker may succeed on Linux or fail closed where nested isolation is unavailable.
		if status == "succeeded" || status == "failed" {
			return
		}
		select {
		case <-deadline:
			t.Fatal("detached execution remained pending")
		case <-time.After(time.Millisecond):
		}
	}
}
