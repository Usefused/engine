package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
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
	body, _ := json.Marshal(mcpCallRequest{OperationID: "session.only.op", Params: map[string]any{}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+sessionID)
	rec := httptest.NewRecorder()
	mcpCallHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (session-scoped operation should resolve then fail validation): %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	// An operation outside the session catalog must not resolve through any
	// process-wide fallback.
	body, _ = json.Marshal(mcpCallRequest{OperationID: "globalOnly.op", Params: map[string]any{}})
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
	body, _ := json.Marshal(mcpCallRequest{OperationID: "test.getWidget", Params: map[string]any{}})
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

	// makePassthroughCache (credential_passthrough_test.go) wires a
	// richMockCache whose GetEndpoint only resolves the hardcoded names
	// "list_items"/"do_thing", and its stub Endpoint doesn't set Method or
	// Parameters -- fine for this test, which is about proving the /mcp/call
	// handler reaches the real engineExecuteCore path end-to-end and
	// forwards the vendor's response, not about path/query/header param
	// routing (that's dispatcher.go's own concern, covered by
	// dispatcher_test.go). Reuse "do_thing" as this fixture's operationId so
	// the same mock cache backs both the fixture-side resolution (this
	// handler) and the scope-side resolution (engineExecuteCore, via
	// findEndpointInScope).
	cache, endpointName := makePassthroughCache(t, vendor.URL)

	fixtureJSON := `{"operations":[{
		"operation_id":"` + endpointName + `",
		"method":"GET",
		"path":"/thing",
		"responses":{"200":{"type":"object"}}
	}]}`
	fixture := fixtureForTest(t, fixtureJSON)

	origCache, origDispatcher, origValidator, origResolver := globalObjectCache, globalDispatcher, globalTokenValidator, globalSecretResolver
	t.Cleanup(func() {
		globalObjectCache, globalDispatcher, globalTokenValidator = origCache, origDispatcher, origValidator
		globalSecretResolver = origResolver
	})
	globalObjectCache = cache
	globalDispatcher = engine.NewDispatcher()
	globalTokenValidator = &dummyTokenValidator{}
	resolver := &mockSecretResolver{creds: map[string]any{"bearerAuth": "server-side-token"}}
	globalSecretResolver = resolver

	sessionID := registerTestMCPSession(t, "tok", fixture)
	resourceID := uuid.NewString()
	mcpSessions.Lock()
	mcpSessions.m[sessionID].authContext = map[string]any{
		"fused_end_user_ref": "customer-42",
		"fused_resource_id":  resourceID,
	}
	mcpSessions.Unlock()

	body, _ := json.Marshal(mcpCallRequest{OperationID: endpointName, Params: map[string]any{}})
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

// assertCallErrorResponse checks the response body is valid JSON with a
// non-empty error field -- i.e. the handler used writeMCPCallResult's
// JSON-safe encoding, not the package's quote-breaking writeError helper.
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
