package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authselector"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// appTestValidator admits the exact version token used by REST and Unified App tests.
type appTestValidator struct{ identity auth.RuntimeIdentity }

// Validate keeps test authentication scoped to one immutable app version.
func (validator appTestValidator) Validate(_ context.Context, appID uuid.UUID, token string) (auth.RuntimeIdentity, error) {
	// A mismatched version or token must fail before any provider dispatch.
	if appID != validator.identity.AppID || token != "fsk_test" {
		return auth.RuntimeIdentity{}, auth.ErrUnauthorized
	}
	return validator.identity, nil
}

type restRuntimeTestDouble struct {
	mu                sync.Mutex
	connects          int
	disconnects       int
	physicalFound     bool
	physicalAmbiguous bool
	physicalErr       error
	selectorErr       error
	physicalResult    sandbox.PhysicalExecutionResult
	physicalCalls     []sandbox.PhysicalExecutionRequest
	resolveBindings   []sandbox.ExactOperationBinding
	resolvedOperation string
}

// TestRESTActionableAuthErrorPreservesSelectorChoices verifies REST does not mask a pre-provider selector correction.
func TestRESTActionableAuthErrorPreservesSelectorChoices(t *testing.T) {
	selectorErr := authselector.NewNotFoundError(
		authselector.Selection{AuthType: "basic", AuthName: "oauth2"},
		[]authselector.Selection{{AuthType: "oauth", AuthName: "googleOAuth"}},
	)
	projected := restActionableAuthError(fmt.Errorf("preflight: %w", selectorErr))
	// REST callers need the same stable code and human-readable exact pair as SDK and MCP callers.
	if projected == nil || projected.status != http.StatusBadRequest || projected.code != "auth_selection_not_found" || !strings.Contains(projected.message, `auth_type="oauth", auth_name="googleOAuth"`) {
		t.Fatalf("REST selector projection = %#v", projected)
	}
}

// TestRESTPhysicalPreflightPreservesSelectorChoices verifies the public handler does not replace a typed auth correction.
func TestRESTPhysicalPreflightPreservesSelectorChoices(t *testing.T) {
	runtime := &restRuntimeTestDouble{
		physicalFound: true,
		selectorErr: authselector.NewNotFoundError(
			authselector.Selection{AuthType: "basic", AuthName: "oauth2"},
			[]authselector.Selection{{AuthType: "oauth", AuthName: "oauth2"}},
		),
	}
	server, appID := newRESTPhysicalServer(runtime)
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"issues.get","input":{},"selector":{"auth_type":"basic","auth_name":"oauth2"}}`, "")
	var envelope restExecutionErrorEnvelope
	decodeErr := json.Unmarshal(response.Body.Bytes(), &envelope)
	// The actual HTTP boundary must retain the stable code and valid pair, not merely the projector unit test.
	if response.Code != http.StatusBadRequest || decodeErr != nil || envelope.Error.Code != "auth_selection_not_found" || !strings.Contains(envelope.Error.Message, `auth_type="oauth", auth_name="oauth2"`) {
		t.Fatalf("REST selector response = %d %s decode=%v", response.Code, response.Body.String(), decodeErr)
	}
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
func (runtime *restRuntimeTestDouble) ValidateResolvedPhysicalSelectors(sandbox.ResolvedPhysicalOperation, sandbox.PhysicalExecutionSelectors) error {
	return runtime.selectorErr
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

// TestRESTExecutionRejectsAmbiguousPhysicalName proves two immutable physical
// matches cannot be resolved by selection order.
func TestRESTExecutionRejectsAmbiguousPhysicalName(t *testing.T) {
	runtime := &restRuntimeTestDouble{physicalAmbiguous: true}
	server, appID := newRESTPhysicalServer(runtime)
	response := performRESTExecution(t, server, appID, "fsk_test", `{"operation":"issues.get","input":{}}`, "")
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

// TestRESTExecutionGraphQLProjectionKeepsSDKKindSeparateFromTransport protects
// SDK activity grouping while raw receipt transport remains REST.
func TestRESTExecutionGraphQLProjectionKeepsSDKKindSeparateFromTransport(t *testing.T) {
	if got := unifiedAppKind(models.EngineExecutionTransportREST); got != string(store.AppKindSDK) {
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
	missingBucket, missingService := uuid.New(), uuid.New()
	tests := []struct {
		err  error
		code string
	}{
		{err: &sandbox.CredentialMaterialMissingError{BucketID: missingBucket, ServiceID: missingService, AuthType: "api_key", AuthName: "providerKey"}, code: "bucket_credentials_missing"},
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
		// Credential absence must include a copyable value-free CLI command.
		if test.code == "bucket_credentials_missing" && (!strings.Contains(recorder.Body.String(), `"command":"fused-cli secret set `+missingService.String()) || !strings.Contains(recorder.Body.String(), `--bucket `+missingBucket.String())) {
			t.Fatalf("credential remediation = %s", recorder.Body.String())
		}
	}
}

// TestRESTExecutionProjectsMissingOAuthApplicationCredentials proves direct
// API callers receive the same value-free setup contract as CLI invocations.
func TestRESTExecutionProjectsMissingOAuthApplicationCredentials(t *testing.T) {
	bucketID, serviceID := uuid.New(), uuid.New()
	executionErr := restErrorFromExecution(&sandbox.CredentialMaterialMissingError{
		BucketID: bucketID, ServiceID: serviceID, AuthType: "oauth", AuthName: "oauth2",
	})
	recorder := httptest.NewRecorder()
	writeRESTExecutionError(recorder, executionErr)
	// Conflict distinguishes mutable credential readiness from malformed app or provider state.
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"code":"bucket_credentials_missing"`) {
		t.Fatalf("OAuth application credential status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	// Routing metadata is sufficient for a client to rebuild the secure prompt without trusting server shell text.
	for _, expected := range []string{`"bucket_id":"` + bucketID.String() + `"`, `"service_id":"` + serviceID.String() + `"`, `"auth_type":"oauth"`, `"auth_name":"oauth2"`} {
		if !strings.Contains(recorder.Body.String(), expected) {
			t.Fatalf("OAuth application credential response missing %s: %s", expected, recorder.Body.String())
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
	server := NewEngineGRPCServer(runtimeStore, nil, nil, nil, nil, appTestValidator{identity: identity}, nil)
	server.restRuntime = runtime
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
