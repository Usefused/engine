package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type restRuntimeTestDouble struct {
	mu                sync.Mutex
	connects          int
	disconnects       int
	physicalFound     bool
	physicalAmbiguous bool
	physicalErr       error
	physicalResult    sandbox.PhysicalExecutionResult
	physicalCalls     []sandbox.PhysicalExecutionRequest
	resolveBindings   []sandbox.ExactOperationBinding
	resolvedOperation string
}

// ConnectAppRuntime records the request-scoped cache acquisition used by the handler.
func (runtime *restRuntimeTestDouble) ConnectAppRuntime(context.Context, uuid.UUID) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.connects++
	return nil
}

// DisconnectAppRuntime records the matching request-scoped cache release.
func (runtime *restRuntimeTestDouble) DisconnectAppRuntime(uuid.UUID) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.disconnects++
}

// ResolvePhysicalOperationByName scripts physical existence without relying on operation syntax.
func (runtime *restRuntimeTestDouble) ResolvePhysicalOperationByName(_ context.Context, _ uuid.UUID, operation string) (sandbox.ResolvedPhysicalOperation, bool, error) {
	runtime.mu.Lock()
	runtime.resolvedOperation = operation
	runtime.mu.Unlock()
	if runtime.physicalAmbiguous {
		return sandbox.ResolvedPhysicalOperation{}, false, sandbox.ErrPhysicalOperationAmbiguous
	}
	return sandbox.ResolvedPhysicalOperation{}, runtime.physicalFound, nil
}

// TestRESTExecutionPhysicalUsesCanonicalOperationLimit proves common routing
// preserves 512-byte physical names while rejecting a 513-byte request.
func TestRESTExecutionPhysicalUsesCanonicalOperationLimit(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true}
	server, appID := newRESTPhysicalServer(runtime)
	acceptedOperation := strings.Repeat("p", maxRESTOperationBytes)
	accepted := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"`+acceptedOperation+`","input":{}}`, "")
	if accepted.Code != http.StatusOK {
		t.Fatalf("512-byte physical operation status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	if runtime.resolvedOperation != acceptedOperation {
		t.Fatalf("resolved operation length=%d, want %d", len(runtime.resolvedOperation), maxRESTOperationBytes)
	}
	rejectedOperation := strings.Repeat("p", maxRESTOperationBytes+1)
	rejected := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"`+rejectedOperation+`","input":{}}`, "")
	assertRESTErrorCode(t, rejected, http.StatusBadRequest, "invalid_request")
	if runtime.connects != 1 || runtime.disconnects != 1 {
		t.Fatalf("oversized operation reached cache lifecycle: %d/%d", runtime.connects, runtime.disconnects)
	}
}

// TestValidateRESTExecutionRequestBoundsPagination proves REST rejects malformed controls before runtime classification or resolution.
func TestValidateRESTExecutionRequestBoundsPagination(t *testing.T) {
	tests := []restExecutionRequest{
		{Operation: "items.list", Input: json.RawMessage(`{}`), Pagination: &restPaginationIntent{MaxPages: 0}},
		{Operation: "items.list", Input: json.RawMessage(`{}`), Pagination: &restPaginationIntent{MaxPages: paginationpolicy.CeilingMaxPages + 1}},
		{Operation: "items.list", Input: json.RawMessage(`{}`), TargetPagination: map[string]*restPaginationIntent{"jira": nil}},
	}
	// Each malformed shape must retain the same bounded public error code.
	for index, request := range tests {
		if err := validateRESTExecutionRequest(request); err == nil || err.code != "pagination_invalid" {
			t.Fatalf("case %d error = %#v", index, err)
		}
	}
}

// TestRESTPhysicalPaginationHashBindsOnlyAtPhysicalBoundary proves pagination participates once in replay conflict identity.
func TestRESTPhysicalPaginationHashBindsOnlyAtPhysicalBoundary(t *testing.T) {
	first := restExecutionRequest{Operation: "items.list", Input: json.RawMessage(`{}`), Pagination: &restPaginationIntent{MaxPages: 1}}
	second := restExecutionRequest{Operation: "items.list", Input: json.RawMessage(`{}`), Pagination: &restPaginationIntent{MaxPages: 2}}
	firstCanonical := []byte(`{"input":{},"operation":"items.list","pagination":{"max_pages":1}}`)
	secondCanonical := []byte(`{"input":{},"operation":"items.list","pagination":{"max_pages":2}}`)
	firstBase := restPhysicalRequestHash(first, firstCanonical)
	secondBase := restPhysicalRequestHash(second, secondCanonical)
	if firstBase != secondBase {
		t.Fatal("REST base hash included pagination before the shared physical binder")
	}
	firstBound := engine.BindPaginationIntentRequestHash(firstBase, runtimeRESTPaginationIntent(first.Pagination))
	secondBound := engine.BindPaginationIntentRequestHash(secondBase, runtimeRESTPaginationIntent(second.Pagination))
	if firstBound == secondBound {
		t.Fatal("different pagination intents produced the same physical replay identity")
	}
}

// ResolveExactPhysicalOperations records Unified's immutable child bindings.
func (runtime *restRuntimeTestDouble) ResolveExactPhysicalOperations(_ context.Context, _ uuid.UUID, bindings []sandbox.ExactOperationBinding) ([]sandbox.ResolvedPhysicalOperation, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.resolveBindings = append([]sandbox.ExactOperationBinding(nil), bindings...)
	return make([]sandbox.ResolvedPhysicalOperation, len(bindings)), nil
}

// ValidateResolvedPhysicalSelectors keeps selector validation on the canonical interface.
func (*restRuntimeTestDouble) ValidateResolvedPhysicalSelectors(sandbox.ResolvedPhysicalOperation, sandbox.PhysicalExecutionSelectors) error {
	return nil
}

// ExecuteResolvedPhysicalJSON records transport and returns deterministic physical or Unified child JSON.
func (runtime *restRuntimeTestDouble) ExecuteResolvedPhysicalJSON(_ context.Context, _ auth.RuntimeIdentity, _ sandbox.ResolvedPhysicalOperation, request sandbox.PhysicalExecutionRequest) (sandbox.PhysicalExecutionResult, error) {
	runtime.mu.Lock()
	runtime.physicalCalls = append(runtime.physicalCalls, request)
	runtime.mu.Unlock()
	if runtime.physicalErr != nil {
		return sandbox.PhysicalExecutionResult{}, runtime.physicalErr
	}
	if runtime.physicalResult.Body != nil {
		return runtime.physicalResult, nil
	}
	if _, isCRM := request.Params["summary"]; isCRM {
		return sandbox.PhysicalExecutionResult{Body: []byte(`{"iid":"crm-1"}`), StatusCode: http.StatusCreated}, nil
	}
	return sandbox.PhysicalExecutionResult{Body: []byte(`{"id":"gh-1"}`), StatusCode: http.StatusCreated}, nil
}

// ExecuteResolvedPhysicalSuccess records REST on rollback children without exposing a body.
func (runtime *restRuntimeTestDouble) ExecuteResolvedPhysicalSuccess(_ context.Context, _ auth.RuntimeIdentity, _ sandbox.ResolvedPhysicalOperation, request sandbox.PhysicalExecutionRequest) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.physicalCalls = append(runtime.physicalCalls, request)
	return runtime.physicalErr
}

// TestRESTExecutionPhysicalUsesExactBearerLifecycleAndCanonicalCore proves the
// route uses exact app auth, paired cache lifecycle, full-intent hashing, and REST transport.
func TestRESTExecutionPhysicalUsesExactBearerLifecycleAndCanonicalCore(t *testing.T) {
	runtime := &restRuntimeTestDouble{
		physicalFound:  true,
		physicalResult: sandbox.PhysicalExecutionResult{Body: []byte(`{"ok":true}`), StatusCode: http.StatusCreated},
	}
	server, appID := newRESTPhysicalServer(runtime)
	first := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"issues.get","input":{"id":7},"selector":{"environment":"sandbox"}}`, "same-key")
	assertRESTPhysicalSuccess(t, first)
	second := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"issues.get","input":{"id":7},"selector":{"environment":"production"}}`, "same-key")
	assertRESTPhysicalSuccess(t, second)
	assertRESTPhysicalRuntime(t, runtime)
}

// assertRESTPhysicalSuccess verifies the stable physical success envelope and
// response hardening independently from runtime-side call accounting.
func assertRESTPhysicalSuccess(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("physical status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", recorder.Header().Get("Cache-Control"))
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["kind"] != "physical" {
		t.Fatalf("physical kind = %#v", response["kind"])
	}
	if response["status_code"] != float64(http.StatusCreated) {
		t.Fatalf("physical status code = %#v", response["status_code"])
	}
}

// assertRESTPhysicalRuntime verifies paired cache ownership, REST transport,
// and full-public-intent hashing for both physical calls.
func assertRESTPhysicalRuntime(t *testing.T, runtime *restRuntimeTestDouble) {
	t.Helper()
	if runtime.connects != 2 {
		t.Fatalf("cache connects = %d, want 2", runtime.connects)
	}
	if runtime.disconnects != 2 {
		t.Fatalf("cache disconnects = %d, want 2", runtime.disconnects)
	}
	if len(runtime.physicalCalls) != 2 {
		t.Fatalf("lifecycle/calls = %d/%d/%d", runtime.connects, runtime.disconnects, len(runtime.physicalCalls))
	}
	if runtime.physicalCalls[0].Transport != models.EngineExecutionTransportREST {
		t.Fatalf("physical transport = %q", runtime.physicalCalls[0].Transport)
	}
	if runtime.physicalCalls[0].RequestBodyHash == "" {
		t.Fatal("physical request lost its replay hash")
	}
	if runtime.physicalCalls[0].RequestBodyHash == runtime.physicalCalls[1].RequestBodyHash {
		t.Fatal("full public selector intent did not participate in replay conflict hashing")
	}
}

// TestRESTExecutionRejectsWrongTokenBeforeCacheConnect proves valid-looking app IDs cannot be used as an existence oracle.
func TestRESTExecutionRejectsWrongTokenBeforeCacheConnect(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true}
	server, appID := newRESTPhysicalServer(runtime)
	response := performRESTExecution(t, server, appID, "wrong", `{"operation":"issues.get","input":{}}`, "")
	assertRESTErrorCode(t, response, http.StatusUnauthorized, "authentication_failed")
	if runtime.connects != 0 || runtime.disconnects != 0 {
		t.Fatalf("unauthenticated request touched cache: %d/%d", runtime.connects, runtime.disconnects)
	}
}

// TestRESTExecutionRejectsRemovedSelectionFieldBeforeCacheConnect proves a
// persisted legacy row cannot reach runtime dispatch through REST.
func TestRESTExecutionRejectsRemovedSelectionFieldBeforeCacheConnect(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true}
	server, appID := newRESTPhysicalServer(runtime)
	scope := server.store.(*grpcRuntimeStore).scope
	scope.Selections = []byte(`[{"service_id":"` + uuid.NewString() + `","service_version_id":"` + uuid.NewString() + `","definition_schema_version":3}]`)

	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"issues.get","input":{}}`, "")
	assertRESTErrorCode(t, response, http.StatusForbidden, "app_scope_unavailable")
	if runtime.connects != 0 || runtime.disconnects != 0 {
		t.Fatalf("legacy selection reached cache lifecycle: %d/%d", runtime.connects, runtime.disconnects)
	}
}

// TestRESTExecutionRejectsSecretShapedSelectorFields proves the strict public DTO cannot become a credential passthrough.
func TestRESTExecutionRejectsSecretShapedSelectorFields(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true}
	server, appID := newRESTPhysicalServer(runtime)
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"issues.get","input":{},"selector":{"api_key":"secret"}}`, "")
	assertRESTErrorCode(t, response, http.StatusBadRequest, "invalid_request")
	if runtime.connects != 0 {
		t.Fatal("invalid strict body reached cache lifecycle")
	}
}

// TestRESTExecutionRejectsPhysicalUnifiedCollision proves targets never decide kind when immutable definitions collide.
func TestRESTExecutionRejectsPhysicalUnifiedCollision(t *testing.T) {
	server, _, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
	runtime := &restRuntimeTestDouble{physicalFound: true}
	server.restRuntime, server.unifiedRuntime = runtime, runtime
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"issues.create","input":{"title":"Bug"},"targets":["github"]}`, "logical-1")
	assertRESTErrorCode(t, response, http.StatusConflict, "operation_ambiguous")
	if len(runtime.physicalCalls) != 0 || runtime.connects != 1 || runtime.disconnects != 1 {
		t.Fatalf("collision dispatch/lifecycle = %d/%d/%d", len(runtime.physicalCalls), runtime.connects, runtime.disconnects)
	}
}

// TestRESTExecutionRejectsAmbiguousPhysicalName proves two immutable physical
// matches cannot be disambiguated by request shape or selection order.
func TestRESTExecutionRejectsAmbiguousPhysicalName(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalAmbiguous: true}
	server, appID := newRESTPhysicalServer(runtime)
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"issues.get","input":{},"targets":["invented"]}`, "")
	assertRESTErrorCode(t, response, http.StatusConflict, "operation_ambiguous")
	if len(runtime.physicalCalls) != 0 {
		t.Fatal("ambiguous physical name reached execution")
	}
}

// TestRESTExecutionRequiresSDKRuntime proves MCP app tokens cannot cross their
// separate session/catalog execution boundary.
func TestRESTExecutionRequiresSDKRuntime(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true}
	server, appID := newRESTPhysicalServer(runtime)
	server.store.(*grpcRuntimeStore).scope.Kind = store.AppKindMCP
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"issues.get","input":{}}`, "")
	assertRESTErrorCode(t, response, http.StatusForbidden, "app_scope_unavailable")
	if runtime.connects != 0 {
		t.Fatal("MCP app reached REST cache lifecycle")
	}
}

// TestRESTExecutionPreservesCanonicalTokenPolicyDenial proves the REST adapter
// maps the physical core's policy decision without provider dispatch details.
func TestRESTExecutionPreservesCanonicalTokenPolicyDenial(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true, physicalErr: sandbox.ErrPhysicalOperationNotAllowed}
	server, appID := newRESTPhysicalServer(runtime)
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"issues.delete","input":{}}`, "")
	assertRESTErrorCode(t, response, http.StatusForbidden, "operation_not_allowed")
}

// TestRESTExecutionAcceptsOnlySafePhysicalSelectors proves all public routing
// fields map to reserved Engine metadata and no arbitrary credential channel exists.
func TestRESTExecutionAcceptsOnlySafePhysicalSelectors(t *testing.T) {
	resourceID := uuid.NewString()
	runtime := &restRuntimeTestDouble{
		physicalFound:  true,
		physicalResult: sandbox.PhysicalExecutionResult{Body: []byte(`{"ok":true}`), StatusCode: http.StatusOK},
	}
	server, appID := newRESTPhysicalServer(runtime)
	body := `{"operation":"issues.get","input":{},"selector":{"environment":"sandbox","end_user_ref":"user-1","auth_type":"oauth","auth_name":"oauthProfile","resource_id":"` + resourceID + `"}}`
	response := performRESTExecution(t, server, appID, "fsk_test", body, "")
	if response.Code != http.StatusOK {
		t.Fatalf("safe selector status=%d body=%s", response.Code, response.Body.String())
	}
	call := runtime.physicalCalls[0]
	if call.Environment != "sandbox" || len(call.Credentials) != 4 || call.Credentials["fused_resource_id"] != resourceID {
		t.Fatalf("safe selector mapping = %#v", call)
	}
}

// TestRESTExecutionCommonInputAllowsAnyJSONValue keeps Unified's existing
// schema authority from being narrowed by the shared REST envelope.
func TestRESTExecutionCommonInputAllowsAnyJSONValue(t *testing.T) {
	request := restExecutionRequest{Operation: "value.inspect", Input: json.RawMessage(`7`)}
	if err := validateRESTExecutionRequest(request); err != nil {
		t.Fatalf("scalar common input rejected before kind inference: %#v", err)
	}
}

// TestRESTExecutionPhysicalRejectsNullInput proves object-only physical params
// cannot be represented by a nil map after otherwise valid JSON decoding.
func TestRESTExecutionPhysicalRejectsNullInput(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true}
	server, appID := newRESTPhysicalServer(runtime)
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"issues.get","input":null}`, "")
	assertRESTErrorCode(t, response, http.StatusBadRequest, "invalid_request")
	if len(runtime.physicalCalls) != 0 {
		t.Fatal("null physical input reached execution")
	}
}

// TestRESTExecutionUnifiedAcceptsScalarWhenItsSchemaDoes proves kind-specific
// Unified schema validation, rather than the common envelope, owns input shape.
func TestRESTExecutionUnifiedAcceptsScalarWhenItsSchemaDoes(t *testing.T) {
	fixture := newUnifiedCompileFixture()
	operation := fixture.document.UnifiedOperations["issues.create"]
	operation.Input = json.RawMessage(`{"type":"integer"}`)
	operation.Output = nil
	fixture.document.UnifiedOperations["issues.create"] = operation
	server, _, appID := newUnifiedRuntimeServerFromFixture(t, fixture, store.AppTokenPolicy{AllowAll: true})
	runtime := &restRuntimeTestDouble{}
	server.restRuntime, server.unifiedRuntime = runtime, runtime
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"issues.create","input":7,"targets":["github","@acme/custom-crm"]}`, "logical-scalar")
	if response.Code != http.StatusOK {
		t.Fatalf("scalar Unified status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf("scalar Unified input was rejected by shared envelope: %s", response.Body.String())
	}
}

// TestRESTExecutionUnifiedMatchesCanonicalResultAndChildTransport proves REST
// uses the same preflight/scheduler and labels every physical child as REST.
func TestRESTExecutionUnifiedMatchesCanonicalResultAndChildTransport(t *testing.T) {
	fixture := newUnifiedCompileFixture()
	operation := fixture.document.UnifiedOperations["issues.create"]
	operation.Output = nil
	fixture.document.UnifiedOperations["issues.create"] = operation
	server, _, appID := newUnifiedRuntimeServerFromFixture(t, fixture, store.AppTokenPolicy{AllowAll: true})
	runtime := &restRuntimeTestDouble{}
	server.restRuntime, server.unifiedRuntime = runtime, runtime
	response := performRESTExecution(t, server, appID, "fsk_test", `{
		"operation":"issues.create","input":{"title":"Bug"},
		"targets":["github","@acme/custom-crm"],
		"selectors":{"github":{"environment":"sandbox","end_user_ref":"user-1","auth_type":"oauth","auth_name":"githubOAuth"}}
	}`, "logical-request-1")
	if response.Code != http.StatusOK {
		t.Fatalf("Unified status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"kind":"unified"`) || !strings.Contains(response.Body.String(), `"rollbacks":[]`) {
		t.Fatalf("Unified envelope = %s", response.Body.String())
	}
	if len(runtime.resolveBindings) != 2 || len(runtime.physicalCalls) != 2 {
		t.Fatalf("Unified parity calls = bindings:%d physical:%d", len(runtime.resolveBindings), len(runtime.physicalCalls))
	}
	for index, call := range runtime.physicalCalls {
		if call.Transport != models.EngineExecutionTransportREST {
			t.Fatalf("Unified child %d transport = %q", index, call.Transport)
		}
	}
}

func TestRESTExecutionUnifiedReturnsExactRootOutput(t *testing.T) {
	server, _, appID := newUnifiedRuntimeServer(t, store.AppTokenPolicy{AllowAll: true})
	runtime := &restRuntimeTestDouble{}
	server.restRuntime, server.unifiedRuntime = runtime, runtime
	response := performRESTExecution(t, server, appID, "fsk_test", `{
		"operation":"issues.create","input":{"title":"Bug"},
		"targets":["github","@acme/custom-crm"]
	}`, "logical-root-output")
	if response.Code != http.StatusOK {
		t.Fatalf("Unified status = %d body=%s", response.Code, response.Body.String())
	}
	if got := strings.TrimSpace(response.Body.String()); got != `{"id":"gh-1"}` {
		t.Fatalf("exact Unified output = %s", got)
	}
	for _, wrapperField := range []string{`"results"`, `"rollbacks"`, `"data"`, `"kind"`} {
		if strings.Contains(response.Body.String(), wrapperField) {
			t.Fatalf("root output retained wrapper field %s: %s", wrapperField, response.Body.String())
		}
	}
}

func TestRESTExecutionUnifiedReturnsBoundedRootOutputError(t *testing.T) {
	fixture := newUnifiedCompileFixture()
	operation := fixture.document.UnifiedOperations["issues.create"]
	operation.Output = json.RawMessage(`{
		"type":"object",
		"properties":{"id":"${response.github.missing}"},
		"required":["id"]
	}`)
	fixture.document.UnifiedOperations["issues.create"] = operation
	server, _, appID := newUnifiedRuntimeServerFromFixture(t, fixture, store.AppTokenPolicy{AllowAll: true})
	runtime := &restRuntimeTestDouble{}
	server.restRuntime, server.unifiedRuntime = runtime, runtime
	response := performRESTExecution(t, server, appID, "fsk_test", `{
		"operation":"issues.create","input":{"title":"Bug"},
		"targets":["github","@acme/custom-crm"]
	}`, "logical-root-error")
	assertRESTErrorCode(t, response, http.StatusUnprocessableEntity, "output_mapping_failed")
	if strings.Contains(response.Body.String(), "missing") || strings.Contains(response.Body.String(), "gh-1") {
		t.Fatalf("root projection error leaked mapping or provider data: %s", response.Body.String())
	}
}

// TestRESTExecutionProjectsNullDataAndExplicitEmptyRollbacks locks JSON null
// semantics instead of dropping a successful provider null value.
func TestRESTExecutionProjectsNullDataAndExplicitEmptyRollbacks(t *testing.T) {
	results := projectRESTUnifiedResults([]*enginev1.UnifiedTargetResult{{
		Target: "provider", Status: "success", DataJson: []byte(`null`),
	}})
	recorder := httptest.NewRecorder()
	writeRESTExecutionJSON(recorder, http.StatusOK, restExecutionSuccess{
		AppID: uuid.NewString(), Operation: "data.read", Kind: "unified",
		Results: results, Rollbacks: []restUnifiedRollback{},
	})
	if !strings.Contains(recorder.Body.String(), `"data":null`) || !strings.Contains(recorder.Body.String(), `"rollbacks":[]`) {
		t.Fatalf("null/rollback envelope = %s", recorder.Body.String())
	}
}

// TestRESTExecutionGraphQLProjectionKeepsSDKKindSeparateFromTransport protects
// SDK activity grouping while raw receipt transport remains REST.
func TestRESTExecutionGraphQLProjectionKeepsSDKKindSeparateFromTransport(t *testing.T) {
	if got := executionAppKind(models.EngineExecutionTransportREST); got != string(store.AppKindSDK) {
		t.Fatalf("REST app kind = %q", got)
	}
}

// TestRESTExecutionReturnsDeterministicNonJSONError proves the REST media
// limitation is explicit and provider bytes never leak through the envelope.
func TestRESTExecutionReturnsDeterministicNonJSONError(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalFound: true, physicalErr: sandbox.ErrPhysicalResponseNotJSON}
	server, appID := newRESTPhysicalServer(runtime)
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"files.download","input":{}}`, "")
	assertRESTErrorCode(t, response, http.StatusBadGateway, "response_not_json")
	if strings.Contains(response.Body.String(), "provider-private-body") {
		t.Fatal("provider body leaked through REST error")
	}
}

// TestRESTExecutionReturnsProviderStatusWithoutBody proves SDK callers receive
// the actionable status while provider-controlled response content stays hidden.
func TestRESTExecutionReturnsProviderStatusWithoutBody(t *testing.T) {
	runtime := &restRuntimeTestDouble{
		physicalFound: true,
		physicalErr:   &sandbox.PhysicalResponseStatusError{StatusCode: http.StatusTooManyRequests},
	}
	server, appID := newRESTPhysicalServer(runtime)
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"items.list","input":{}}`, "")
	assertRESTErrorCode(t, response, http.StatusBadGateway, "provider_error")
	// The numeric status is safe and sufficient for callers to recognize throttling.
	if !strings.Contains(response.Body.String(), `"provider_http_status":429`) {
		t.Fatalf("provider status missing from REST error: %s", response.Body.String())
	}
	// Provider response bytes remain outside the public error contract.
	if strings.Contains(response.Body.String(), "provider-private-body") {
		t.Fatalf("provider body leaked through REST error: %s", response.Body.String())
	}
}

// TestRESTExecutionProjectsActionableErrorsWithSafeDetails locks connection and
// environment repair metadata without admitting provider bodies.
func TestRESTExecutionProjectsActionableErrorsWithSafeDetails(t *testing.T) {
	tests := []struct {
		err  error
		code string
	}{
		{err: &sandbox.ConnectionRequiredError{Code: "connection_required", BucketID: uuid.NewString(), ServiceID: uuid.NewString(), EndUserRef: "user-1"}, code: "connection_required"},
		{err: &sandbox.ReconnectRequiredError{Code: "reconnect_required", BucketID: uuid.NewString(), ServiceID: uuid.NewString(), ConnectionID: uuid.NewString(), Reason: "refresh_rejected"}, code: "reconnect_required"},
		{err: &sandbox.ResourceSelectionRequiredError{Code: "resource_selection_required", BucketID: uuid.NewString(), ServiceID: uuid.NewString(), ConnectionID: uuid.NewString(), Reason: "multiple_resources"}, code: "resource_selection_required"},
		{err: &sandbox.EnvironmentNotSupportedError{Code: "environment_not_supported", Requested: "test", Available: []string{"production"}}, code: "environment_not_supported"},
	}
	for _, test := range tests {
		executionErr := restErrorFromExecution(test.err)
		recorder := httptest.NewRecorder()
		writeRESTExecutionError(recorder, executionErr)
		if !strings.Contains(recorder.Body.String(), `"code":"`+test.code+`"`) || !strings.Contains(recorder.Body.String(), `"details":`) {
			t.Fatalf("actionable error %s = %s", test.code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "provider-private-body") {
			t.Fatalf("actionable error leaked provider data: %s", recorder.Body.String())
		}
	}
}

// newRESTPhysicalServer builds one exact SDK app plus an injectable in-process runtime.
func newRESTPhysicalServer(runtime *restRuntimeTestDouble) (*EngineGRPCServer, uuid.UUID) {
	accountID, appID := uuid.New(), uuid.New()
	selections, _ := json.Marshal([]models.SDKSelection{{
		ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SchemaVersion: models.AppSelectionSchemaVersion,
	}})
	scope := &store.AppRuntime{
		AccountID: accountID, AppID: appID, BucketID: uuid.New(), Kind: store.AppKindSDK,
		ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: selections,
	}
	identity := auth.RuntimeIdentity{
		AccountID: accountID, AppFamilyID: uuid.New(), AppID: appID, AppVersion: "1.0.0",
		Kind: store.AppKindSDK, Status: store.AppStatusActive, TokenPolicy: store.AppTokenPolicy{AllowAll: true},
	}
	runtimeStore := &grpcRuntimeStore{Store: &workspaceTestStore{}, accountID: accountID, appID: appID, scope: scope}
	server := NewEngineGRPCServer(runtimeStore, nil, nil, nil, nil, unifiedTestValidator{identity: identity})
	server.restRuntime, server.unifiedRuntime = runtime, runtime
	return server, appID
}

// performRESTExecution sends one authenticated request through the mounted chi route.
func performRESTExecution(t *testing.T, server *EngineGRPCServer, appID uuid.UUID, token, body, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	MountAppExecutionRoute(router, server)
	request := httptest.NewRequest(http.MethodPost, "/v1/apps/"+appID.String()+"/executions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// assertRESTErrorCode verifies the stable envelope without coupling tests to messages.
func assertRESTErrorCode(t *testing.T, recorder *httptest.ResponseRecorder, statusCode int, code string) {
	t.Helper()
	if recorder.Code != statusCode {
		t.Fatalf("status = %d want %d body=%s", recorder.Code, statusCode, recorder.Body.String())
	}
	var envelope restExecutionErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != code {
		t.Fatalf("error code = %q want %q body=%s", envelope.Error.Code, code, recorder.Body.String())
	}
}
