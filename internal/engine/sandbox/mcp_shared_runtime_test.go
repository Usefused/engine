package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fixtureForTest loads through the same parser used for serialized session
// catalogs, keeping handler tests realistic without global fixture state.
func fixtureForTest(t *testing.T, raw string) *Fixture {
	t.Helper()
	fixture, err := LoadFixture(writeTempFixture(t, raw))
	if err != nil {
		t.Fatalf("LoadFixture() error = %v", err)
	}
	return fixture
}

// registerTestMCPSession inserts a session directly into the transport-neutral
// registry both MCP handlers populate in production, so
// mcpCallHandler's session lookup has something real to find. Returns the
// session ID to use as the request's bearer token, and registers cleanup.
func registerTestMCPSession(t *testing.T, token string, fixture *Fixture) string {
	t.Helper()
	sessionID := uuid.NewString()
	mcpSessions.Lock()
	mcpSessions.m[sessionID] = &mcpSession{
		appID:           uuid.NewString(),
		sessionID:       sessionID,
		token:           token,
		pendingRequests: make(map[string]struct{}),
		fixture:         fixture,
	}
	mcpSessions.Unlock()
	t.Cleanup(func() {
		mcpSessions.Lock()
		delete(mcpSessions.m, sessionID)
		mcpSessions.Unlock()
	})
	return sessionID
}

// TestMCPCallHandlerUnavailableSessionReturnsTypedRecovery keeps the private bridge from collapsing session loss to prose.
func TestMCPCallHandlerUnavailableSessionReturnsTypedRecovery(t *testing.T) {
	sessionID := uuid.NewString()
	request := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewBufferString(`{"operation_id":"fixture.read","params":{}}`))
	request.Header.Set("Authorization", "Bearer "+sessionID)
	response := httptest.NewRecorder()
	mcpCallHandler(response, request)

	var body mcpCallResponse
	// The bridge exposes only the agent decision, while transport state remains internal telemetry.
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != http.StatusNotFound || body.Code != "MCP_BRIDGE_SESSION_UNAVAILABLE" || body.RecoveryAction != "reinitialize_connection" || body.ExecuteRequest != "reformat_if_session_state_used" || body.ProviderExecution != "not_started" || body.AutomaticReplay == nil || *body.AutomaticReplay {
		t.Fatalf("bridge session recovery = status:%d body:%+v raw:%s", response.Code, body, response.Body.String())
	}
	if strings.Contains(response.Body.String(), sessionID) {
		t.Fatalf("bridge response exposed session identity: %s", response.Body.String())
	}
}

// TestMcpCallHandlerUsesOnlySessionFixture proves the artifact-derived catalog
// is both the discovery surface and the first dispatch authorization boundary.
func TestMcpCallHandlerUsesOnlySessionFixture(t *testing.T) {
	// The session-only operation declares a required param that this test
	// deliberately omits, so a resolved-but-rejected 400 (schema validation
	// failure) proves Tier-1 resolution found it in the session fixture,
	// without needing dispatchMCPCall's globalObjectCache/globalDispatcher
	// wiring (out of scope for this test -- see
	// TestMcpCallHandler_EndToEndDispatchesThroughEngineExecuteCore for that).
	sessionFixture := newFixtureFromOperations(context.Background(), []FixtureOperation{{
		OperationID: "session.only.op",
		Method:      "GET",
		Path:        "/session/{id}",
		Parameters:  []models.Parameter{{Name: "id", In: "path", Required: true}},
		Responses:   models.Responses{"200": {Representations: []models.ResponseRepresentation{{Schema: &models.SchemaContract{Projection: models.Schema{Type: "object"}}}}}},
	}})
	sessionID := registerTestMCPSession(t, "tok", sessionFixture)

	// The session-only operation must resolve (400: found, rejected for a
	// missing param -- not 404, which would mean it wasn't found at all).
	body, _ := json.Marshal(mcpCallRequest{OperationID: "session.only.op", Params: json.RawMessage(`{}`)})
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+sessionID)
	rec := httptest.NewRecorder()
	mcpCallHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (session-scoped operation should resolve then fail validation): %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	// An operation outside the session catalog must not resolve through any
	// process-wide fallback.
	body, _ = json.Marshal(mcpCallRequest{OperationID: "globalOnly.op", Params: json.RawMessage(`{}`)})
	req = httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+sessionID)
	rec = httptest.NewRecorder()
	mcpCallHandler(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (global-only op should not resolve for a session with its own fixture)", rec.Code, http.StatusNotFound)
	}
}

func TestExtractBearerToken(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		setHeader bool
		wantToken string
		wantOK    bool
	}{
		{name: "valid bearer", header: "Bearer abc123", setHeader: true, wantToken: "abc123", wantOK: true},
		{name: "missing header", setHeader: false, wantToken: "", wantOK: false},
		{name: "empty bearer value", header: "Bearer ", setHeader: true, wantToken: "", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/mcp/call", nil)
			if tc.setHeader {
				r.Header.Set("Authorization", tc.header)
			}
			token, ok := extractBearerToken(r)
			if ok != tc.wantOK || token != tc.wantToken {
				t.Errorf("extractBearerToken() = (%q, %v), want (%q, %v)", token, ok, tc.wantToken, tc.wantOK)
			}
		})
	}
}

func TestBufferToJSONResult(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{name: "empty body becomes null", in: nil, want: "null"},
		{name: "valid JSON forwarded as-is", in: []byte(`{"ok":true}`), want: `{"ok":true}`},
		{name: "non-JSON text is JSON-encoded as a string", in: []byte("plain text"), want: `"plain text"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bufferToJSONResult(tc.in)
			if err != nil {
				t.Fatalf("bufferToJSONResult() error = %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("bufferToJSONResult() = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestBufferToBoundedJSONResultRejectsExpandedText proves the model-visible
// JSON remains bounded even when string escaping expands a provider body.
func TestBufferToBoundedJSONResultRejectsExpandedText(t *testing.T) {
	raw := bytes.Repeat([]byte{0}, maxMCPPhysicalResultBytes/2)
	_, err := bufferToBoundedJSONResult(raw, maxMCPPhysicalResultBytes)
	// A stable sentinel lets the handler discard wrapper text and payload content.
	if !errors.Is(err, engine.ErrBufferStreamLimitExceeded) {
		t.Fatalf("bufferToBoundedJSONResult() error = %v, want ErrBufferStreamLimitExceeded", err)
	}
}

// TestMCPCallWriterBoundsFailureText prevents error paths from bypassing the
// same wire-size budget used for successful physical results.
func TestMCPCallWriterBoundsFailureText(t *testing.T) {
	recorder := httptest.NewRecorder()
	message := string(bytes.Repeat([]byte{0}, maxMCPPhysicalResultBytes/2))
	writeMCPCallResult(recorder, http.StatusBadGateway, mcpCallResponse{Error: message})
	// Escaped error text must be replaced completely, without retaining provider bytes.
	if got := recorder.Body.String(); got != "{\"error\":\"mcp_call_result_too_large\"}\n" {
		t.Fatalf("bounded error body = %s", got)
	}
}

// TestDecodeMCPPhysicalPaginationIntentUsesCanonicalBounds proves the bridge does not create a second pagination policy.
func TestDecodeMCPPhysicalPaginationIntentUsesCanonicalBounds(t *testing.T) {
	tests := []struct {
		name    string
		value   *mcpPhysicalPaginationIntent
		want    int
		wantErr bool
	}{
		{name: "omitted"},
		{name: "one page", value: &mcpPhysicalPaginationIntent{MaxPages: 1}, want: 1},
		{name: "zero", value: &mcpPhysicalPaginationIntent{}, wantErr: true},
		{name: "above ceiling", value: &mcpPhysicalPaginationIntent{MaxPages: paginationpolicy.CeilingMaxPages + 1}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			intent, err := decodeMCPPhysicalPaginationIntent(test.value)
			// Error presence and the decoded value together distinguish every public shape.
			if (err != nil) != test.wantErr {
				t.Fatalf("decode error = %v, wantErr %v", err, test.wantErr)
			}
			// Omission must remain nil so automatic pagination retains its full policy.
			if test.value == nil {
				if intent != nil {
					t.Fatalf("omitted intent = %#v, want nil", intent)
				}
				return
			}
			// Only valid controls produce a canonical Engine value.
			if !test.wantErr && (intent == nil || intent.MaxPages != test.want) {
				t.Fatalf("decoded intent = %#v, want max pages %d", intent, test.want)
			}
		})
	}
}

// TestContextWithMCPPhysicalExecutionIdentityBindsPagination proves replay identity includes caller result-size intent.
func TestContextWithMCPPhysicalExecutionIdentityBindsPagination(t *testing.T) {
	params := map[string]any{"userId": "me"}
	without := contextWithMCPPhysicalExecutionIdentity(context.Background(), params, nil)
	with := contextWithMCPPhysicalExecutionIdentity(context.Background(), params, &engine.PaginationIntent{MaxPages: 1})
	intent, ok := engine.PaginationIntentFromContext(with)
	// A present option must reach the canonical Dispatcher context unchanged.
	if !ok || intent.MaxPages != 1 {
		t.Fatalf("pagination intent = %#v/%v", intent, ok)
	}
	// Equal provider params with different pagination semantics cannot share replay identity.
	if requestBodyHashFromContext(without) == requestBodyHashFromContext(with) {
		t.Fatal("pagination intent did not change the request hash")
	}
}

// TestBoundedMCPPhysicalCallErrorExplainsPaginationFailures keeps stable codes and bounded remediation at the script boundary.
func TestBoundedMCPPhysicalCallErrorExplainsPaginationFailures(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		want       string
	}{
		{name: "invalid intent", err: &engine.PaginationIntentValidationError{Reason: engine.PaginationIntentInvalidValue}, wantStatus: http.StatusBadRequest, want: "mcp_pagination_max_pages_invalid"},
		{name: "page limit", err: &engine.PaginationError{Code: "max_pages"}, wantStatus: http.StatusUnprocessableEntity, want: "mcp_pagination_max_pages"},
		{name: "duration limit", err: &engine.PaginationError{Code: "max_duration"}, wantStatus: http.StatusGatewayTimeout, want: "mcp_pagination_max_duration"},
		{name: "unsafe continuation", err: &engine.PaginationError{Code: "untrusted_next_url"}, wantStatus: http.StatusBadGateway, want: "mcp_pagination_untrusted_next_url"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			statusCode, message := boundedMCPPhysicalCallError("gmail.users.messages.get", test.err)
			// The fixed prefix remains machine-readable while the suffix carries recovery guidance.
			if statusCode != test.wantStatus || !bytes.HasPrefix([]byte(message), []byte(test.want+":")) {
				t.Fatalf("mapped failure = %d/%q, want %d/%q prefix", statusCode, message, test.wantStatus, test.want)
			}
		})
	}
}

// TestBoundedMCPPaginationIntentErrorProvidesExactCallCorrection pins agent-readable guidance for every policy decision.
func TestBoundedMCPPaginationIntentErrorProvidesExactCallCorrection(t *testing.T) {
	tests := []struct {
		name string
		err  *engine.PaginationIntentValidationError
		want string
	}{
		{name: "invalid value", err: &engine.PaginationIntentValidationError{Reason: engine.PaginationIntentInvalidValue}, want: `mcp_pagination_max_pages_invalid: operation "gmail.users.messages.get" received an invalid pagination.maxPages; use a positive integer lower than pagination.engine_max_pages only when search_docs reports pagination.caller_bound_supported=true, otherwise use call("gmail.users.messages.get", params) without a pagination option`},
		{name: "not supported", err: &engine.PaginationIntentValidationError{Reason: engine.PaginationIntentNotSupported}, want: `mcp_pagination_not_supported: operation "gmail.users.messages.get" is not paginated; use call("gmail.users.messages.get", params) without a pagination option`},
		{name: "lower bound available", err: &engine.PaginationIntentValidationError{Reason: engine.PaginationIntentBoundNotLower, EngineMaxPages: 10}, want: `mcp_pagination_bound_not_lower: operation "gmail.users.messages.get" has an Engine page limit of 10; use pagination.maxPages between 1 and 9, or omit pagination`},
		{name: "no lower bound", err: &engine.PaginationIntentValidationError{Reason: engine.PaginationIntentBoundNotLower, EngineMaxPages: 1}, want: `mcp_pagination_bound_not_lower: operation "gmail.users.messages.get" has an Engine page limit of 1, so no lower positive pagination.maxPages exists; use call("gmail.users.messages.get", params) without a pagination option`},
		{name: "unknown future reason", err: &engine.PaginationIntentValidationError{Reason: "future"}, want: mcpPaginationIntentUnknown},
	}
	// Exact strings make the error a stable executable recovery contract rather than advisory prose.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Any wording drift can reintroduce agent guesswork even when the stable prefix remains unchanged.
			if got := boundedMCPPaginationIntentError("gmail.users.messages.get", test.err); got != test.want {
				t.Fatalf("pagination guidance = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMcpCallHandler_MissingAuthorizationRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()

	mcpCallHandler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertCallErrorResponse(t, rec)
}

func TestMcpCallHandler_UnknownSessionRejected(t *testing.T) {
	body, _ := json.Marshal(mcpCallRequest{OperationID: "anything"})
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer not-a-real-session")
	rec := httptest.NewRecorder()

	mcpCallHandler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertCallErrorResponse(t, rec)
}

// TestMcpCallHandlerRejectsOversizedChildRuntimePayload proves the private
// call bridge cannot bypass the external MCP message admission budget.
func TestMcpCallHandlerRejectsOversizedChildRuntimePayload(t *testing.T) {
	sessionID := registerTestMCPSession(t, "tok", fixtureForTest(t, validFixtureJSON))
	body := bytes.Repeat([]byte("x"), maxMCPMessageBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+sessionID)
	rec := httptest.NewRecorder()

	mcpCallHandler(rec, req)

	// Payload rejection stays exact and contains none of the submitted body.
	if rec.Code != http.StatusRequestEntityTooLarge || rec.Body.String() != "{\"error\":\"mcp_call_payload_too_large\"}\n" {
		t.Fatalf("oversized call response = %d/%s", rec.Code, rec.Body.String())
	}
}

func TestMcpCallHandler_UnknownOperationIdRejected(t *testing.T) {
	sessionID := registerTestMCPSession(t, "tok", fixtureForTest(t, validFixtureJSON))

	// Tier-1 enforcement (design doc, Trust and Governance Model): this
	// operationId is well-formed but was never registered in the fixture,
	// so it must be rejected before touching a session, credential, or
	// vendor.
	body, _ := json.Marshal(mcpCallRequest{OperationID: "never.registered"})
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+sessionID)
	rec := httptest.NewRecorder()

	mcpCallHandler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d, body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	assertCallErrorResponse(t, rec)
}

func TestMcpCallHandler_SchemaValidationFailureRejected(t *testing.T) {
	sessionID := registerTestMCPSession(t, "tok", fixtureForTest(t, validFixtureJSON))

	// test.getWidget requires a path param "id" (see validFixtureJSON in
	// mcp_fixture_test.go) -- omitting it must fail validation before
	// engineExecuteCore is ever reached, per the design doc's "Guarding
	// Against Hallucinated Calls" backstop.
	body, _ := json.Marshal(mcpCallRequest{OperationID: "test.getWidget", Params: json.RawMessage(`{}`)})
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+sessionID)
	rec := httptest.NewRecorder()

	mcpCallHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	assertCallErrorResponse(t, rec)
}

// TestMcpCallHandler_EndToEndDispatchesThroughEngineExecuteCore is the
// authoritative test for Task 5/6's wiring: a registered session, a
// resolvable+valid operationId, dispatched through the exact same
// engineExecuteCore path the gRPC edge uses, reaching a real (test) vendor
// server and returning its response as the call() result.
func TestMcpCallHandler_EndToEndDispatchesThroughEngineExecuteCore(t *testing.T) {
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"received":true}`))
	}))
	defer vendor.Close()

	sessionID, endpointName, resolver := configureMCPPhysicalCallTest(t, vendor.URL)
	resourceID := uuid.NewString()
	mcpSessions.Lock()
	mcpSessions.m[sessionID].authContext = map[string]any{
		"fused_end_user_ref": "customer-42",
		"fused_resource_id":  resourceID,
	}
	mcpSessions.Unlock()

	body, _ := json.Marshal(mcpCallRequest{OperationID: endpointName, Params: json.RawMessage(`{}`)})
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+sessionID)
	rec := httptest.NewRecorder()

	mcpCallHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp mcpCallResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("resp.Error = %q, want empty", resp.Error)
	}
	if string(resp.Result) != `{"received":true}` {
		t.Errorf("resp.Result = %s, want vendor body forwarded verbatim", resp.Result)
	}
	if resolver.passthrough["fused_end_user_ref"] != "customer-42" || resolver.passthrough["fused_resource_id"] != resourceID {
		t.Fatalf("MCP middleware lost connected-user context: %#v", resolver.passthrough)
	}
}

type mcpPaginatedCache struct {
	*richMockCache
	pagination *fusedobject.PaginationConfig
}

// GetEndpoint adds pagination to the shared physical fixture without changing its scope, auth, or transport behavior.
func (cache *mcpPaginatedCache) GetEndpoint(ctx context.Context, appID, serviceID, endpointName string) (*fusedobject.Endpoint, error) {
	endpoint, err := cache.richMockCache.GetEndpoint(ctx, appID, serviceID, endpointName)
	// The base fixture remains authoritative for exact operation admission.
	if err != nil {
		return nil, err
	}
	cloned := *endpoint
	cloned.Pagination = cache.pagination
	return &cloned, nil
}

// TestMcpCallHandlerAppliesPhysicalPaginationBeforeReturning proves standard and caller-bounded traversal stay inside Engine and precede result handling.
func TestMcpCallHandlerAppliesPhysicalPaginationBeforeReturning(t *testing.T) {
	providerCalls := 0
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		providerCalls++
		// The continuation query proves Engine, rather than the MCP session layer, owns traversal.
		if request.URL.Query().Get("cursor") == "page-two" {
			_, _ = w.Write([]byte(`{"items":[{"id":"older"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"latest"}],"next":"page-two"}`))
	}))
	defer vendor.Close()

	sessionID, endpointName, _ := configureMCPPhysicalCallTest(t, vendor.URL)
	baseCache := globalObjectCache.(*richMockCache)
	baseCache.parameters = fusedobject.Parameters{{Name: "cursor", In: "query", Type: "string"}}
	globalObjectCache = &mcpPaginatedCache{richMockCache: baseCache, pagination: testCursorPagination("cursor", "$.next")}

	statusCode, response := executeMCPPhysicalHandlerTest(t, sessionID, endpointName, nil)
	// Omission must exhaust standard provider pagination and publish only the completed aggregate.
	if statusCode != http.StatusOK || providerCalls != 2 || string(response.Result) != `{"items":[{"id":"latest"},{"id":"older"}]}` {
		t.Fatalf("automatic pagination = %d/%s, provider calls = %d", statusCode, response.Result, providerCalls)
	}

	providerCalls = 0
	statusCode, _ = executeMCPPhysicalHandlerTest(t, sessionID, endpointName, &mcpPhysicalPaginationIntent{MaxPages: paginationpolicy.DefaultMaxPages})
	// Equal policy limits are rejected before provider I/O because only strict reductions can return partial results.
	if statusCode != http.StatusBadRequest || providerCalls != 0 {
		t.Fatalf("non-tightening intent = %d, provider calls = %d", statusCode, providerCalls)
	}

	statusCode, response = executeMCPPhysicalHandlerTest(t, sessionID, endpointName, &mcpPhysicalPaginationIntent{MaxPages: 1})
	// One provider page must complete and aggregate before the bridge exposes its successful result.
	if statusCode != http.StatusOK || providerCalls != 1 {
		t.Fatalf("physical pagination = %d/%s, provider calls = %d", statusCode, response.Result, providerCalls)
	}
	// Consumed continuation state is Engine metadata and must not survive in the returned provider document.
	if string(response.Result) != `{"items":[{"id":"latest"}]}` {
		t.Fatalf("paginated result = %s", response.Result)
	}
}

// TestMcpCallHandlerExplainsPhysicalPaginationIntentErrors proves actionable guidance crosses the real bridge without provider work.
func TestMcpCallHandlerExplainsPhysicalPaginationIntentErrors(t *testing.T) {
	providerCalls := 0
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer vendor.Close()

	sessionID, endpointName, _ := configureMCPPhysicalCallTest(t, vendor.URL)
	baseCache := globalObjectCache.(*richMockCache)
	globalObjectCache = &mcpPaginatedCache{richMockCache: baseCache, pagination: testCursorPagination("cursor", "$.next")}
	statusCode, response := executeMCPPhysicalHandlerTest(t, sessionID, endpointName, &mcpPhysicalPaginationIntent{MaxPages: paginationpolicy.DefaultMaxPages})
	wantBound := fmt.Sprintf(`mcp_pagination_bound_not_lower: operation %q has an Engine page limit of %d; use pagination.maxPages between 1 and %d, or omit pagination`, endpointName, paginationpolicy.DefaultMaxPages, paginationpolicy.DefaultMaxPages-1)
	assertMCPPaginationIntentRejection(t, statusCode, response, providerCalls, wantBound)

	globalObjectCache = baseCache
	statusCode, response = executeMCPPhysicalHandlerTest(t, sessionID, endpointName, &mcpPhysicalPaginationIntent{MaxPages: 1})
	wantUnsupported := fmt.Sprintf(`mcp_pagination_not_supported: operation %q is not paginated; use call(%q, params) without a pagination option`, endpointName, endpointName)
	assertMCPPaginationIntentRejection(t, statusCode, response, providerCalls, wantUnsupported)

	statusCode, response = executeMCPPhysicalHandlerTest(t, sessionID, endpointName, &mcpPhysicalPaginationIntent{})
	wantInvalid := fmt.Sprintf(`mcp_pagination_max_pages_invalid: operation %q received an invalid pagination.maxPages; use a positive integer lower than pagination.engine_max_pages only when search_docs reports pagination.caller_bound_supported=true, otherwise use call(%q, params) without a pagination option`, endpointName, endpointName)
	assertMCPPaginationIntentRejection(t, statusCode, response, providerCalls, wantInvalid)
}

// assertMCPPaginationIntentRejection verifies exact public guidance and the pre-provider admission boundary.
func assertMCPPaginationIntentRejection(t *testing.T, statusCode int, response mcpCallResponse, providerCalls int, want string) {
	t.Helper()
	// Pagination intent mistakes are caller errors and must never trigger a provider request.
	if statusCode != http.StatusBadRequest || response.Error != want || providerCalls != 0 {
		t.Fatalf("pagination rejection = %d/%q, provider calls = %d; want %q", statusCode, response.Error, providerCalls, want)
	}
}

// executeMCPPhysicalHandlerTest drives one authenticated bridge request and decodes its bounded response.
func executeMCPPhysicalHandlerTest(t *testing.T, sessionID, endpointName string, pagination *mcpPhysicalPaginationIntent) (int, mcpCallResponse) {
	t.Helper()
	body, err := json.Marshal(mcpCallRequest{OperationID: endpointName, Params: json.RawMessage(`{}`), Pagination: pagination})
	// Test setup must stop before dispatch if the trusted request fixture cannot encode.
	if err != nil {
		t.Fatalf("encode MCP physical request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+sessionID)
	recorder := httptest.NewRecorder()
	mcpCallHandler(recorder, request)
	var response mcpCallResponse
	// Error and success envelopes share one bounded JSON response contract.
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode MCP physical response: %v", err)
	}
	return recorder.Code, response
}

// TestMcpCallHandlerRejectsOversizedPhysicalResult proves vendor output cannot
// cross the child-runtime bridge or leave a partial result in its error body.
func TestMcpCallHandlerRejectsOversizedPhysicalResult(t *testing.T) {
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxMCPPhysicalResultBytes+1))
	}))
	defer vendor.Close()

	sessionID, endpointName, _ := configureMCPPhysicalCallTest(t, vendor.URL)
	body, _ := json.Marshal(mcpCallRequest{OperationID: endpointName, Params: json.RawMessage(`{}`)})
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+sessionID)
	rec := httptest.NewRecorder()

	mcpCallHandler(rec, req)

	// The static code is safe for scripts while provider bytes remain unobservable.
	if rec.Code != http.StatusBadGateway || rec.Body.String() != "{\"error\":\"mcp_call_result_too_large\"}\n" {
		t.Fatalf("oversized physical result response = %d/%s", rec.Code, rec.Body.String())
	}
}

// configureMCPPhysicalCallTest wires one real physical dispatch through the
// production cache, validator, resolver, dispatcher, and session boundaries.
func configureMCPPhysicalCallTest(t *testing.T, vendorURL string) (string, string, *mockSecretResolver) {
	t.Helper()
	// The shared cache fixture keeps handler tests on the canonical execution path.
	cache, endpointName := makePassthroughCache(t, vendorURL)
	fixtureJSON := `{"operations":[{
		"operation_id":"` + endpointName + `",
		"method":"GET",
		"path":"/thing",
		"responses":{"200":{"type":"object"}}
	}]}`
	fixture := fixtureForTest(t, fixtureJSON)
	originalCache, originalDispatcher := globalObjectCache, globalDispatcher
	originalValidator, originalResolver := globalTokenValidator, globalSecretResolver
	// Cleanup prevents these process-owned dependencies from leaking into sibling tests.
	t.Cleanup(func() {
		globalObjectCache, globalDispatcher = originalCache, originalDispatcher
		globalTokenValidator, globalSecretResolver = originalValidator, originalResolver
	})
	globalObjectCache = cache
	globalDispatcher = engine.NewDispatcher()
	globalTokenValidator = &dummyTokenValidator{}
	resolver := &mockSecretResolver{creds: map[string]any{"bearerAuth": "server-side-token"}}
	globalSecretResolver = resolver
	return registerTestMCPSession(t, "tok", fixture), endpointName, resolver
}

// TestMcpCallHandler_ExecutesUnifiedThroughCanonicalCoordinator proves one
// discovered logical name reaches the injected ExecuteUnified method with
// trusted MCP transport, metadata, and SDK-equivalent options.
func TestMcpCallHandler_ExecutesUnifiedThroughCanonicalCoordinator(t *testing.T) {
	fixture := unifiedFixtureForTest(t, "release.provision")
	sessionID := registerTestMCPSession(t, "family-token", fixture)
	var captured *enginev1.ExecuteUnifiedRequest
	var capturedTransport string
	var capturedMetadata metadata.MD
	previous := globalMCPUnifiedExecute
	// Restore the process-owned coordinator so parallel package tests cannot inherit this fixture.
	t.Cleanup(func() { globalMCPUnifiedExecute = previous })
	// The test coordinator records the adapter contract and returns the same
	// protobuf response shape the production scheduler owns.
	globalMCPUnifiedExecute = func(ctx context.Context, request *enginev1.ExecuteUnifiedRequest) (*enginev1.ExecuteUnifiedResponse, error) {
		captured, capturedTransport = request, ExecutionTransportFromContext(ctx)
		capturedMetadata, _ = metadata.FromIncomingContext(ctx)
		return &enginev1.ExecuteUnifiedResponse{Results: []*enginev1.UnifiedTargetResult{{
			Target: "github", Status: "success", DataJson: []byte(`{"id":1}`),
		}}, RollbackResults: []*enginev1.UnifiedRollbackResult{{
			Target: "github", Status: "error", ErrorCode: "rollback_failed", TriggeredBy: []string{"gitlab"},
			AuthAction: &enginev1.UnifiedAuthAction{Action: "reconnect", BucketId: "bucket", ServiceId: "service", EndUserRef: "user"},
		}}}, nil
	}

	body := []byte(`{"operation_id":"release.provision","params":{"input":{"count":9007199254740993},"targets":["github"],"selectors":{"github":{"endUserRef":"user","authType":"oauth"}},"pagination":{"github":{"maxPages":2}}}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+sessionID)
	rec := httptest.NewRecorder()
	mcpCallHandler(rec, req)
	// A successful adapter response proves no physical fallback consumed the logical name.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if captured == nil || captured.Operation != "release.provision" || string(captured.InputJson) != `{"count":9007199254740993}` {
		t.Fatalf("captured Unified request = %#v", captured)
	}
	apiKeys := capturedMetadata.Get("x-api-key")
	// Trusted metadata must be attached independently of model-authored params.
	if capturedTransport != models.EngineExecutionTransportMCP || len(apiKeys) != 1 || apiKeys[0] != "family-token" {
		t.Fatalf("trusted transport/metadata = %q/%#v", capturedTransport, capturedMetadata)
	}
	if captured.TargetSelectors["github"].GetEndUserRef() != "user" || captured.TargetPagination["github"].GetMaxPages() != 2 {
		t.Fatalf("SDK-equivalent options were not forwarded: %#v", captured)
	}
	// Omitted idempotency defaults once per logical call, matching generated SDKs.
	if _, err := uuid.Parse(captured.IdempotencyKey); err != nil {
		t.Fatalf("generated idempotency key = %q", captured.IdempotencyKey)
	}
	var response mcpCallResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Unified response: %v", err)
	}
	encoded := string(response.Result)
	if !bytes.Contains(response.Result, []byte(`"data":{"id":1}`)) || bytes.Contains(response.Result, []byte("connectionId")) || bytes.Contains(response.Result, []byte("reason")) {
		t.Fatalf("SDK-compatible all-settled result = %s", encoded)
	}
}

// TestDecodeMCPUnifiedInvocationRejectsNonSDKShapes locks strict camelCase
// options and one-document decoding before trusted metadata is attached.
func TestDecodeMCPUnifiedInvocationRejectsNonSDKShapes(t *testing.T) {
	cases := []string{
		`{"input":{},"targets":["github"],"unexpected":true}`,
		`{"input":{},"targets":["github"],"selectors":{"github":{"end_user_ref":"user"}}}`,
		`{"input":{},"targets":["github"],"pagination":{"github":{"max_pages":2}}}`,
		`{"input":{},"targets":["github"]} {}`,
	}
	for _, raw := range cases {
		// Every alternate spelling or suffix must fail instead of being ignored.
		if _, err := decodeMCPUnifiedInvocation(json.RawMessage(raw)); err == nil {
			t.Fatalf("decodeMCPUnifiedInvocation(%s) error = nil", raw)
		}
	}
	invocation, err := decodeMCPUnifiedInvocation(json.RawMessage(`{"input":{},"targets":["github"],"idempotencyKey":" "}`))
	if err != nil || invocation.IdempotencyKey != " " {
		t.Fatalf("present whitespace key was rewritten: %#v, %v", invocation, err)
	}
}

// TestMcpCallHandlerBoundsUnifiedCoordinatorErrors ensures private runtime
// messages never cross the MCP adapter or enter model-visible script errors.
func TestMcpCallHandlerBoundsUnifiedCoordinatorErrors(t *testing.T) {
	sessionID := registerTestMCPSession(t, "family-token", unifiedFixtureForTest(t, "release.provision"))
	previous := globalMCPUnifiedExecute
	// Restore the process-owned coordinator after exercising the bounded failure path.
	t.Cleanup(func() { globalMCPUnifiedExecute = previous })
	// The private status message simulates definition or provider context that must be discarded.
	globalMCPUnifiedExecute = func(context.Context, *enginev1.ExecuteUnifiedRequest) (*enginev1.ExecuteUnifiedResponse, error) {
		return nil, status.Error(codes.PermissionDenied, "private selector and provider details")
	}
	body := []byte(`{"operation_id":"release.provision","params":{"input":{},"targets":["github"]}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+sessionID)
	rec := httptest.NewRecorder()
	mcpCallHandler(rec, req)
	if rec.Code != http.StatusForbidden || rec.Body.String() != "{\"error\":\"unified_execution_denied\"}\n" {
		t.Fatalf("bounded coordinator error = %d/%s", rec.Code, rec.Body.String())
	}
}

// TestProjectMCPUnifiedResponseMatchesSDKSemantics locks final-output authority
// and the generated SDK's bounded all-settled fallbacks.
func TestProjectMCPUnifiedResponseMatchesSDKSemantics(t *testing.T) {
	descriptor := &models.SDKUnifiedOperationDescriptor{OutputSchema: json.RawMessage(`{"type":"object"}`)}
	result, statusCode, code := projectMCPUnifiedResponse(descriptor, &enginev1.ExecuteUnifiedResponse{OutputJson: []byte(`{"id":"root"}`)})
	if string(result) != `{"id":"root"}` || statusCode != 0 || code != "" {
		t.Fatalf("configured output projection = %s/%d/%q", result, statusCode, code)
	}
	_, statusCode, code = projectMCPUnifiedResponse(descriptor, &enginev1.ExecuteUnifiedResponse{})
	// A configured output never degrades into an all-settled response.
	if statusCode != http.StatusUnprocessableEntity || code != "output_unavailable" {
		t.Fatalf("missing configured output = %d/%q", statusCode, code)
	}
	result, statusCode, code = projectMCPUnifiedResponse(nil, &enginev1.ExecuteUnifiedResponse{
		Results:         []*enginev1.UnifiedTargetResult{{Target: "ok", Status: "success"}, {Target: "skip", Status: "skipped"}, {Target: "bad", Status: "unexpected"}},
		RollbackResults: []*enginev1.UnifiedRollbackResult{{Target: "bad", Status: "unexpected"}},
	})
	// Empty bodies and absent codes receive the same null/default projections as generated clients.
	want := `{"results":[{"target":"ok","status":"success","data":null,"errorCode":null,"authAction":null},{"target":"skip","status":"skipped","data":null,"errorCode":"dependency_failed","authAction":null},{"target":"bad","status":"error","data":null,"errorCode":"execution_failed","authAction":null}],"rollbacks":[{"target":"bad","status":"error","errorCode":"rollback_failed","triggeredBy":[],"authAction":null}]}`
	if string(result) != want || statusCode != 0 || code != "" {
		t.Fatalf("all-settled projection = %s/%d/%q", result, statusCode, code)
	}
}

// unifiedFixtureForTest attaches one exact public descriptor through the same
// collision and schema admission used by production session construction.
func unifiedFixtureForTest(t *testing.T, operation string) *Fixture {
	t.Helper()
	fixture := newFixtureFromOperations(context.Background(), nil)
	descriptor := &models.SDKUnifiedOperationDescriptors{SchemaVersion: models.SDKUnifiedDescriptorSchemaVersion, Operations: []models.SDKUnifiedOperationDescriptor{{
		Name: operation, InputSchema: json.RawMessage(`{"type":"object"}`), Targets: []models.SDKUnifiedTargetDescriptor{{PublicTarget: "github", OperationID: "repos.create"}},
	}}}
	if err := fixture.attachUnifiedOperations(descriptor); err != nil {
		t.Fatalf("attach Unified descriptor: %v", err)
	}
	return fixture
}

// assertCallErrorResponse checks the response body is valid JSON with a
// non-empty error field, preserving the bridge's typed shape independently of shared HTTP errors.
func assertCallErrorResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var resp mcpCallResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response body is not valid JSON: %v (body = %s)", err, rec.Body.String())
	}
	if resp.Error == "" {
		t.Error("resp.Error = \"\", want a non-empty error message")
	}
}
