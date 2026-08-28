package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type streamableTokenValidator struct {
	token   string
	tokenID uuid.UUID
}

func (validator *streamableTokenValidator) Validate(_ context.Context, appID uuid.UUID, token string) (auth.RuntimeIdentity, error) {
	if token != validator.token {
		return auth.RuntimeIdentity{}, auth.ErrUnauthorized
	}
	return auth.RuntimeIdentity{
		AppID: appID, TokenID: validator.tokenID,
		TokenPolicy: store.AppTokenPolicy{AllowAll: true},
	}, nil
}

type streamableWriteBuffer struct {
	bytes.Buffer
}

// Close lets the in-memory writer satisfy the child stdin lifecycle contract.
func (*streamableWriteBuffer) Close() error { return nil }

type streamableFailingWriter struct {
	written int
}

type streamableSessionCache struct {
	*richMockCache
}

// ListEndpointsForSelections returns one empty batch because the transport contract needs no provider fixture.
func (*streamableSessionCache) ListEndpointsForSelections(context.Context, []models.SDKSelection, []string) (map[int][]fusedobject.Endpoint, error) {
	return map[int][]fusedobject.Endpoint{}, nil
}

// MCPPaginationForSelections returns no policies because this transport-only catalogue has no operations.
func (*streamableSessionCache) MCPPaginationForSelections(context.Context, []models.SDKSelection) (map[int]*fusedobject.PaginationConfig, error) {
	return map[int]*fusedobject.PaginationConfig{}, nil
}

// GetMCPUnifiedOperationDescriptors returns the canonical absent logical catalogue for this transport-only test.
func (*streamableSessionCache) GetMCPUnifiedOperationDescriptors(context.Context, string, store.AppTokenPolicy) (*models.SDKUnifiedOperationDescriptors, error) {
	return nil, nil
}

// GetMCPServerMetadata supplies stable identity for transport tests that do not load an applied app plan.
func (*streamableSessionCache) GetMCPServerMetadata(context.Context, string) (FixtureServerMetadata, error) {
	return FixtureServerMetadata{Name: "transport-test", Title: "Transport test", Version: "1.0.0", Description: "Exercise the MCP transport contract."}, nil
}

// Write simulates a dead or partially consumed child pipe without retaining request content.
func (writer *streamableFailingWriter) Write(payload []byte) (int, error) {
	// The configured prefix is capped to the supplied payload so tests cannot violate io.Writer's byte-count contract.
	if writer.written > len(payload) {
		return len(payload), io.ErrClosedPipe
	}
	return writer.written, io.ErrClosedPipe
}

// Close completes the test writer contract without owning external resources.
func (*streamableFailingWriter) Close() error { return nil }

func TestMCPStreamablePostReturnsMatchingRuntimeResponse(t *testing.T) {
	sess, input, token := installStreamableSessionTestState(t)
	sess.responses <- `{"jsonrpc":"2.0","id":7,"result":{"tools":[]}}`
	response := serveStreamableTestRequest(t, sess, token, http.MethodPost, `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)

	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"id":7`)) {
		t.Fatalf("status/body = %d/%s, want matching JSON-RPC response", response.Code, response.Body.String())
	}
	if got := input.String(); got != "{\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"tools/list\"}\n" {
		t.Fatalf("runtime input = %q, want one newline-delimited request", got)
	}
}

func TestMCPStreamableRequestRevalidatesToken(t *testing.T) {
	sess, _, _ := installStreamableSessionTestState(t)
	response := serveStreamableTestRequest(t, sess, "wrong-token", http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", response.Code, response.Body.String())
	}
	if _, ok := lookupMCPSession(sess.sessionID); !ok {
		t.Fatal("a denied request unexpectedly terminated the valid session")
	}
}

func TestMCPTransportsRejectEachOthersSessionIDs(t *testing.T) {
	streamable, _, token := installStreamableSessionTestState(t)
	sseMessage := httptest.NewRequest(http.MethodPost, "/mcp/message?sessionId="+streamable.sessionID, bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	sseMessage.Header.Set("Authorization", "Bearer "+token)
	sseResponse := httptest.NewRecorder()
	mcpMessageHandler(sseResponse, sseMessage)
	if sseResponse.Code != http.StatusNotFound {
		t.Fatalf("SSE endpoint accepted Streamable session: %d", sseResponse.Code)
	}

	sse := &mcpSession{
		appID: streamable.appID, sessionID: uuid.NewString(), tokenID: streamable.tokenID,
		protocolVersion: "2024-11-05", transport: "sse", token: token,
		idleTimer: time.AfterFunc(time.Hour, func() {}),
	}
	mcpSessions.Lock()
	mcpSessions.m[sse.sessionID] = sse
	mcpSessions.Unlock()
	t.Cleanup(func() { terminateMCPSession(sse.sessionID, "test_cleanup") })
	streamableResponse := serveStreamableTestRequest(t, sse, token, http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if streamableResponse.Code != http.StatusNotFound {
		t.Fatalf("Streamable endpoint accepted SSE session: %d", streamableResponse.Code)
	}
}

func TestMCPStreamableTelemetryRecordsSafeOutcome(t *testing.T) {
	exporter := installStreamableTestTracer(t)
	sess, _, token := installStreamableSessionTestState(t)
	sess.responses <- `{"jsonrpc":"2.0","id":8,"result":{}}`
	response := serveStreamableTestRequest(t, sess, token, http.MethodPost, `{"jsonrpc":"2.0","id":8,"method":"tools/list"}`)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	assertSafeStreamableSpan(t, exporter.GetSpans(), token, sess.sessionID)
}

func TestMCPStreamableDeleteEndsSessionWithoutRevokingToken(t *testing.T) {
	sess, _, token := installStreamableSessionTestState(t)
	response := serveStreamableTestRequest(t, sess, token, http.MethodDelete, "")

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", response.Code, response.Body.String())
	}
	if _, ok := lookupMCPSession(sess.sessionID); ok {
		t.Fatal("deleted transport session is still active")
	}
	if _, err := validateMCPToken(context.Background(), sess.appID, token); err != nil {
		t.Fatalf("DELETE revoked the independent execution token: %v", err)
	}
}

func TestMCPSessionTokenInvalidatorEndsOnlyMatchingSessions(t *testing.T) {
	sess, _, _ := installStreamableSessionTestState(t)
	other := &mcpSession{
		appID: uuid.NewString(), sessionID: uuid.NewString(), tokenID: uuid.New(),
		idleTimer: time.AfterFunc(time.Hour, func() {}),
	}
	mcpSessions.Lock()
	mcpSessions.m[other.sessionID] = other
	mcpSessions.Unlock()
	t.Cleanup(func() { terminateMCPSession(other.sessionID, "test_cleanup") })

	if got := (MCPSessionTokenInvalidator{}).InvalidateToken(sess.tokenID); got != 1 {
		t.Fatalf("terminated sessions = %d, want 1", got)
	}
	if _, ok := lookupMCPSession(sess.sessionID); ok {
		t.Fatal("revoked-token session is still active")
	}
	if _, ok := lookupMCPSession(other.sessionID); !ok {
		t.Fatal("unrelated-token session was terminated")
	}
}

// TestMCPStreamableFirstPostRequiresInitialize keeps pre-session requests on the client-owned handshake path.
func TestMCPStreamableFirstPostRequiresInitialize(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":null,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/mcp/"+uuid.NewString(), bytes.NewBufferString(body))
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		streamableTestRouter().ServeHTTP(response, request)

		// Neither a non-initialize request nor an uncorrelated null ID may allocate a session.
		if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte("initialize is required")) || !bytes.Contains(response.Body.Bytes(), []byte(mcpSessionInitializeRequiredCode)) {
			t.Fatalf("status/body = %d/%s, want initialize error", response.Code, response.Body.String())
		}
	}
}

// TestMCPStreamableStaleSessionReturnsRecoveryContract proves a pre-dispatch 404 never asks the agent to guess session state.
func TestMCPStreamableStaleSessionReturnsRecoveryContract(t *testing.T) {
	sess, _, token := installStreamableSessionTestState(t)
	terminateMCPSession(sess.sessionID, "test_cleanup")
	response := serveStreamableTestRequest(t, sess, token, http.MethodPost, `{"jsonrpc":"2.0","id":17,"method":"tools/list"}`)

	assertMCPFailureResponse(t, response, http.StatusNotFound, "17", mcpSessionFailureData{
		Code: mcpSessionUnavailableCode, RecoveryAction: "reinitialize_connection",
		ExecuteRequest: "reformat_if_session_state_used", ProviderExecution: "not_started", AutomaticReplay: false,
	})
	// No response may expose the opaque transport identifier or bearer material.
	if strings.Contains(response.Body.String(), sess.sessionID) || strings.Contains(response.Body.String(), token) {
		t.Fatalf("session recovery exposed transport identity: %s", response.Body.String())
	}
}

// TestMCPStreamableProtocolMismatchReturnsRecoveryContract keeps negotiated transport state out of model guesses.
func TestMCPStreamableProtocolMismatchReturnsRecoveryContract(t *testing.T) {
	sess, _, token := installStreamableSessionTestState(t)
	request := httptest.NewRequest(http.MethodPost, "/mcp/"+sess.appID, bytes.NewBufferString(`{"jsonrpc":"2.0","id":23,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(mcpSessionIDHeader, sess.sessionID)
	request.Header.Set(mcpProtocolVersionHeader, "2099-wrong")
	response := httptest.NewRecorder()
	streamableTestRouter().ServeHTTP(response, request)

	assertMCPFailureResponse(t, response, http.StatusBadRequest, "23", mcpSessionFailureData{
		Code: mcpSessionProtocolMismatchCode, RecoveryAction: "use_negotiated_protocol",
		ExecuteRequest: "unchanged", ProviderExecution: "not_started", AutomaticReplay: false,
	})
}

// TestMCPStreamableGetWithoutSessionReturnsTypedHeaderGuidance prevents clients from inventing an identifier.
func TestMCPStreamableGetWithoutSessionReturnsTypedHeaderGuidance(t *testing.T) {
	sess, _, token := installStreamableSessionTestState(t)
	request := httptest.NewRequest(http.MethodGet, "/mcp/"+sess.appID, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	streamableTestRouter().ServeHTTP(response, request)

	var body struct {
		Data mcpSessionFailureData `json:"data"`
	}
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != http.StatusBadRequest || body.Data.Code != mcpSessionHeaderRequiredCode || body.Data.RecoveryAction != "initialize_connection" || body.Data.ExecuteRequest != "unchanged" || body.Data.ProviderExecution != "not_started" {
		t.Fatalf("missing session header recovery = status:%d body:%s", response.Code, response.Body.String())
	}
}

// TestMCPStreamableDispatchFailureTerminatesSession distinguishes zero-byte rejection from an uncertain partial write.
func TestMCPStreamableDispatchFailureTerminatesSession(t *testing.T) {
	for _, test := range []struct {
		name, code, recovery, executeRequest, providerExecution string
		written                                                 int
		mayExecute                                              bool
	}{
		{name: "zero byte protocol request", code: mcpRuntimeDispatchFailedCode, recovery: "reinitialize_connection", executeRequest: "unchanged", providerExecution: "not_started"},
		{name: "zero byte execute", code: mcpRuntimeDispatchFailedCode, recovery: "reinitialize_connection", executeRequest: "reformat_if_session_state_used", providerExecution: "not_started", mayExecute: true},
		{name: "partial protocol request", code: mcpRuntimeDispatchFailedCode, recovery: "reinitialize_connection", executeRequest: "unchanged", providerExecution: "not_started", written: 1},
		{name: "partial execute", code: mcpRuntimeOutcomeUnknownCode, recovery: "do_not_replay", executeRequest: "do_not_replay", providerExecution: "unknown", written: 1, mayExecute: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sess, _, token := installStreamableSessionTestState(t)
			sess.stdin = &streamableFailingWriter{written: test.written}
			body := `{"jsonrpc":"2.0","id":18,"method":"tools/list"}`
			// Only execute can make a partially delivered request provider-side-effect-uncertain.
			if test.mayExecute {
				body = `{"jsonrpc":"2.0","id":18,"method":"tools/call","params":{"name":"execute","arguments":{"script":"return await call('mutate', {});"}}}`
			}
			response := serveStreamableTestRequest(t, sess, token, http.MethodPost, body)

			assertMCPFailureResponse(t, response, http.StatusBadGateway, "18", mcpSessionFailureData{
				Code: test.code, RecoveryAction: test.recovery,
				ExecuteRequest: test.executeRequest, ProviderExecution: test.providerExecution, AutomaticReplay: false,
			})
			// An unusable child cannot remain discoverable as an active session after dispatch failure.
			if _, ok := lookupMCPSession(sess.sessionID); ok {
				t.Fatal("dispatch failure left the session active")
			}
		})
	}
}

// TestMCPSSEDispatchFailureIsTypedAndAudited keeps compatibility transport failures on the same recovery and OTEL contract.
func TestMCPSSEDispatchFailureIsTypedAndAudited(t *testing.T) {
	exporter := installStreamableTestTracer(t)
	sess, _, token := installStreamableSessionTestState(t)
	sess.transport = "sse"
	sess.stdin = &streamableFailingWriter{}
	request := httptest.NewRequest(http.MethodPost, "/mcp/message?sessionId="+sess.sessionID, bytes.NewBufferString(`{"jsonrpc":"2.0","id":19,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	mcpMessageHandler(response, request)

	assertMCPFailureResponse(t, response, http.StatusBadGateway, "19", mcpSessionFailureData{
		Code: mcpRuntimeDispatchFailedCode, RecoveryAction: "reinitialize_connection",
		ExecuteRequest: "unchanged", ProviderExecution: "not_started", AutomaticReplay: false,
	})
	// Failure telemetry may retain bounded state only, never transport identity or bearer material.
	assertMCPFailureSpan(t, exporter.GetSpans(), "engine.sandbox.mcp.sse_message", token, sess.sessionID, mcpRuntimeDispatchFailedCode)
}

// TestMCPStreamableInitializeUsesNegotiatedVersion publishes headers only for a valid child InitializeResult.
func TestMCPStreamableInitializeUsesNegotiatedVersion(t *testing.T) {
	sess, _, _ := installStreamableSessionTestState(t)
	sess.protocolVersion = "2099-unsupported-request"
	sess.responses <- `{"jsonrpc":"2.0","id":21,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"fixture","version":"1"}}}`
	response := httptest.NewRecorder()
	request := mcpJSONRPCRequest{JSONRPC: "2.0", ID: json.RawMessage("21"), Method: "initialize", Params: json.RawMessage(`{"protocolVersion":"2099-unsupported-request","capabilities":{}}`)}
	_, span := otel.Tracer("test").Start(context.Background(), "initialize")
	accepted := serveMCPStreamableRequest(context.Background(), span, response, sess, []byte(`{"jsonrpc":"2.0","id":21,"method":"initialize","params":{"protocolVersion":"2099-unsupported-request","capabilities":{}}}`), request)
	span.End()

	if !accepted || response.Code != http.StatusOK || response.Header().Get(mcpSessionIDHeader) != sess.sessionID || response.Header().Get(mcpProtocolVersionHeader) != "2025-06-18" || sess.protocolVersion != "2025-06-18" {
		t.Fatalf("negotiated initialize = accepted:%v status:%d session:%q header-version:%q stored-version:%q body:%s", accepted, response.Code, response.Header().Get(mcpSessionIDHeader), response.Header().Get(mcpProtocolVersionHeader), sess.protocolVersion, response.Body.String())
	}
}

// TestMCPStreamableInvalidInitializeResultAdvertisesNoSession keeps failed handshakes from publishing a dead identifier.
func TestMCPStreamableInvalidInitializeResultAdvertisesNoSession(t *testing.T) {
	sess, _, _ := installStreamableSessionTestState(t)
	sess.responses <- `{"jsonrpc":"2.0","id":22,"error":{"code":-32603,"message":"failed"}}`
	response := httptest.NewRecorder()
	request := mcpJSONRPCRequest{JSONRPC: "2.0", ID: json.RawMessage("22"), Method: "initialize", Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{}}`)}
	_, span := otel.Tracer("test").Start(context.Background(), "initialize")
	accepted := serveMCPStreamableRequest(context.Background(), span, response, sess, []byte(`{"jsonrpc":"2.0","id":22,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`), request)
	span.End()

	// Invalid child negotiation is a read-only runtime failure, so execute formatting and provider state remain unchanged.
	if accepted || response.Header().Get(mcpSessionIDHeader) != "" || response.Header().Get(mcpProtocolVersionHeader) != "" {
		t.Fatalf("invalid initialize advertised session: accepted:%v headers:%v body:%s", accepted, response.Header(), response.Body.String())
	}
	assertMCPFailureResponse(t, response, http.StatusBadGateway, "22", mcpSessionFailureData{
		Code: mcpRuntimeResponseFailedCode, RecoveryAction: "reinitialize_connection",
		ExecuteRequest: "unchanged", ProviderExecution: "not_started", AutomaticReplay: false,
	})
}

// TestMCPStreamableSessionContractEndToEnd drives the full initialized lifecycle, tools, state, deletion, and typed reconnect through a real child runtime.
func TestMCPStreamableSessionContractEndToEnd(t *testing.T) {
	// The shipped MCP runtime is a Node bundle, so environments without Node cannot exercise this transport boundary.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required for Streamable MCP end-to-end coverage")
	}
	previousValidator, previousCache, previousEnginePort, previousConfig := globalTokenValidator, globalObjectCache, globalEnginePort, cfg
	localConfig := *cfg
	// Race instrumentation can push real Node startup beyond the package-wide one-second unit-test deadline.
	localConfig.Sandbox.ToolCallTimeoutSeconds = 10
	cfg = &localConfig
	appID, token, tokenID := uuid.NewString(), "end-to-end-token", uuid.New()
	selection := []models.SDKSelection{{
		ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SchemaVersion: models.AppSelectionSchemaVersion,
	}}
	scopeJSON, err := json.Marshal(selection)
	// The session fixture must cross the same current selection-schema admission as a deployed MCP version.
	if err != nil {
		t.Fatal(err)
	}
	globalTokenValidator = &streamableTokenValidator{token: token, tokenID: tokenID}
	globalObjectCache = &streamableSessionCache{richMockCache: &richMockCache{scopeJSON: scopeJSON}}
	globalEnginePort = "1"
	t.Cleanup(func() {
		globalTokenValidator, globalObjectCache, globalEnginePort, cfg = previousValidator, previousCache, previousEnginePort, previousConfig
	})

	initialize := serveMCPStreamableEndToEndRequest(t, appID, token, "", "", `{"jsonrpc":"2.0","id":31,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"session-e2e","version":"1"}}}`)
	sessionID, protocolVersion := assertMCPStreamableEndToEndInitialize(t, initialize)
	t.Cleanup(func() { terminateMCPSession(sessionID, "test_cleanup") })

	initialized := serveMCPStreamableEndToEndRequest(t, appID, token, sessionID, protocolVersion, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	assertMCPStreamableEndToEndResponse(t, initialized, http.StatusAccepted, "initialized notification")
	// Notifications have no JSON-RPC response, so acceptance must not synthesize a result envelope.
	if initialized.Body.Len() != 0 {
		t.Fatalf("initialized notification returned a body: %s", initialized.Body.String())
	}

	listed := serveMCPStreamableEndToEndRequest(t, appID, token, sessionID, protocolVersion, `{"jsonrpc":"2.0","id":32,"method":"tools/list","params":{}}`)
	assertMCPStreamableEndToEndResponse(t, listed, http.StatusOK, "tools/list session contract", `"com.usefused/session"`, `"session_id_input":false`)
	executed := serveMCPStreamableEndToEndRequest(t, appID, token, sessionID, protocolVersion, `{"jsonrpc":"2.0","id":33,"method":"tools/call","params":{"name":"execute","arguments":{"script":"session.set(\"proof\",\"active\"); return {proof:session.get(\"proof\")};"}}}`)
	assertMCPStreamableEndToEndResponse(t, executed, http.StatusOK, "execute retained state", `\"proof\":\"active\"`)

	deleted := serveMCPStreamableEndToEndRequest(t, appID, token, sessionID, protocolVersion, "")
	assertMCPStreamableEndToEndResponse(t, deleted, http.StatusNoContent, "DELETE")
	stale := serveMCPStreamableEndToEndRequest(t, appID, token, sessionID, protocolVersion, `{"jsonrpc":"2.0","id":34,"method":"tools/list","params":{}}`)
	assertMCPFailureResponse(t, stale, http.StatusNotFound, "34", mcpSessionFailureData{
		Code: mcpSessionUnavailableCode, RecoveryAction: "reinitialize_connection",
		ExecuteRequest: "reformat_if_session_state_used", ProviderExecution: "not_started", AutomaticReplay: false,
	})
}

// assertMCPStreamableEndToEndInitialize verifies child negotiation and returns the transport-owned headers for later requests.
func assertMCPStreamableEndToEndInitialize(t *testing.T, response *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	sessionID, protocolVersion := response.Header().Get(mcpSessionIDHeader), response.Header().Get(mcpProtocolVersionHeader)
	// A successful child negotiation is the only point at which the transport may expose its opaque session header.
	if sessionID == "" || protocolVersion != "2025-03-26" {
		t.Fatalf("initialize returned invalid headers: status:%d headers:%v body:%s", response.Code, response.Header(), response.Body.String())
	}
	assertMCPStreamableEndToEndResponse(t, response, http.StatusOK, "initialize", "already attached every execute call")
	return sessionID, protocolVersion
}

// assertMCPStreamableEndToEndResponse checks the HTTP result and every protocol marker expected from the real child.
func assertMCPStreamableEndToEndResponse(t *testing.T, response *httptest.ResponseRecorder, status int, step string, required ...string) {
	t.Helper()
	// Status is checked independently so a misleading body cannot satisfy the transport assertion.
	if response.Code != status {
		t.Fatalf("%s failed: %d/%s", step, response.Code, response.Body.String())
	}
	for _, marker := range required {
		// Every marker captures a distinct contract promise made visible to the connected client.
		if !strings.Contains(response.Body.String(), marker) {
			t.Fatalf("%s omitted %q: %d/%s", step, marker, response.Code, response.Body.String())
		}
	}
}

// TestMCPStreamablePostRejectsOversizedPayload proves admission stops before JSON parsing or session creation.
func TestMCPStreamablePostRejectsOversizedPayload(t *testing.T) {
	appID := uuid.NewString()
	request := httptest.NewRequest(http.MethodPost, "/mcp/"+appID, strings.NewReader(strings.Repeat("x", maxMCPMessageBodyBytes+1)))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()

	streamableTestRouter().ServeHTTP(response, request)

	// HTTP 413 is the transport-level signal before an untrusted envelope reaches the runtime.
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "mcp_message_payload_too_large") {
		t.Fatalf("oversized payload response = %d/%s", response.Code, response.Body.String())
	}
}

func TestInitializeMCPProtocolVersionRejectsMalformedValues(t *testing.T) {
	for _, params := range []string{
		`{}`,
		`{"protocolVersion":"invalid version"}`,
		`{"protocolVersion":"` + strings.Repeat("x", 33) + `"}`,
	} {
		if _, err := initializeMCPProtocolVersion([]byte(params)); err == nil {
			t.Fatalf("accepted malformed initialize params: %s", params)
		}
	}
}

func TestRegisterMCPRoutesKeepsSSEAndStreamableHTTP(t *testing.T) {
	router := chi.NewRouter()
	registerMCPRoutes(router)
	tests := []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: "/mcp/" + uuid.NewString() + "/sse", want: http.StatusUnauthorized},
		{method: http.MethodPost, path: "/mcp/" + uuid.NewString(), want: http.StatusUnauthorized},
		{method: http.MethodGet, path: "/mcp/" + uuid.NewString(), want: http.StatusUnauthorized},
		{method: http.MethodDelete, path: "/mcp/" + uuid.NewString(), want: http.StatusUnauthorized},
		{method: http.MethodPost, path: "/mcp/message", want: http.StatusBadRequest},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.want {
			t.Fatalf("%s %s status = %d, want %d", test.method, test.path, response.Code, test.want)
		}
	}
}

// TestWriteErrorEscapesJSON verifies shared HTTP failures remain parseable with quoted dependency text.
func TestWriteErrorEscapesJSON(t *testing.T) {
	response := httptest.NewRecorder()
	writeError(response, http.StatusBadRequest, `quoted "detail"`)
	var body map[string]string
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || body["error"] != `quoted "detail"` {
		t.Fatalf("shared error was not valid JSON: %s", response.Body.String())
	}
}

func installStreamableSessionTestState(t *testing.T) (*mcpSession, *streamableWriteBuffer, string) {
	t.Helper()
	previousValidator, previousCache := globalTokenValidator, globalObjectCache
	token, tokenID, input := "execution-token", uuid.New(), &streamableWriteBuffer{}
	globalTokenValidator = &streamableTokenValidator{token: token, tokenID: tokenID}
	globalObjectCache = nil
	sess := &mcpSession{
		appID: uuid.NewString(), sessionID: uuid.NewString(), tokenID: tokenID,
		protocolVersion: "2025-06-18", transport: mcpStreamableTransport,
		token: token, stdin: input, responses: make(chan string, 2),
		pendingRequests: make(map[string]struct{}), idleTimer: time.AfterFunc(time.Hour, func() {}),
	}
	mcpSessions.Lock()
	mcpSessions.m[sess.sessionID] = sess
	mcpSessions.Unlock()
	t.Cleanup(func() {
		terminateMCPSession(sess.sessionID, "test_cleanup")
		globalTokenValidator, globalObjectCache = previousValidator, previousCache
	})
	return sess, input, token
}

func serveStreamableTestRequest(t *testing.T, sess *mcpSession, token, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/mcp/"+sess.appID, bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(mcpSessionIDHeader, sess.sessionID)
	request.Header.Set(mcpProtocolVersionHeader, sess.protocolVersion)
	response := httptest.NewRecorder()
	streamableTestRouter().ServeHTTP(response, request)
	return response
}

// serveMCPStreamableEndToEndRequest sends one real Engine transport request and applies session headers only when supplied.
func serveMCPStreamableEndToEndRequest(t *testing.T, appID, token, sessionID, protocolVersion, body string) *httptest.ResponseRecorder {
	t.Helper()
	method := http.MethodPost
	// An empty body represents the protocol's explicit session termination request.
	if body == "" {
		method = http.MethodDelete
	}
	request := httptest.NewRequest(method, "/mcp/"+appID, bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer "+token)
	// Initialization intentionally omits both transport-owned headers.
	if sessionID != "" {
		request.Header.Set(mcpSessionIDHeader, sessionID)
		request.Header.Set(mcpProtocolVersionHeader, protocolVersion)
	}
	response := httptest.NewRecorder()
	streamableTestRouter().ServeHTTP(response, request)
	return response
}

func streamableTestRouter() http.Handler {
	router := chi.NewRouter()
	router.HandleFunc("/mcp/{id}", mcpStreamableHandler)
	return router
}

func installStreamableTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	return exporter
}

func assertSafeStreamableSpan(t *testing.T, spans []tracetest.SpanStub, token, sessionID string) {
	t.Helper()
	for _, span := range spans {
		if span.Name != "engine.sandbox.mcp.streamable_http" {
			continue
		}
		attributes := make(map[string]string, len(span.Attributes))
		for _, item := range span.Attributes {
			value := item.Value.Emit()
			if strings.Contains(value, token) || strings.Contains(value, sessionID) {
				t.Fatalf("span attribute %q exposed credential or session identity", item.Key)
			}
			attributes[string(item.Key)] = value
		}
		if attributes["outcome"] != "success" || attributes["http.request.method"] != http.MethodPost {
			t.Fatalf("streamable span attributes = %#v", attributes)
		}
		return
	}
	t.Fatal("streamable HTTP span was not emitted")
}

// assertMCPFailureResponse validates only the compact, non-secret recovery fields shared across transports.
func assertMCPFailureResponse(t *testing.T, response *httptest.ResponseRecorder, status int, id string, want mcpSessionFailureData) {
	t.Helper()
	var envelope struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Data mcpSessionFailureData `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode MCP failure: %v: %s", err, response.Body.String())
	}
	// Public responses must guide the agent without copying the richer OTEL classification fields.
	if response.Code != status || string(envelope.ID) != id || !reflect.DeepEqual(envelope.Error.Data, want) {
		t.Fatalf("MCP failure = status:%d id:%s data:%+v, want status:%d id:%s data:%+v", response.Code, envelope.ID, envelope.Error.Data, status, id, want)
	}
	assertMCPFieldsAbsent(t, response.Body.String(), []string{"session_state", "request_delivery", "side_effect_state", "phase", "remediation"})
}

// assertMCPFieldsAbsent keeps internal diagnostics out of every model-visible recovery assertion.
func assertMCPFieldsAbsent(t *testing.T, body string, fields []string) {
	t.Helper()
	for _, field := range fields {
		// Internal transport details remain bounded OTEL dimensions and must not expand model-visible errors.
		if strings.Contains(body, `"`+field+`"`) {
			t.Fatalf("MCP failure exposed internal field %q: %s", field, body)
		}
	}
}

// assertMCPFailureSpan enforces the closed transport-failure telemetry vocabulary and its identity denylist.
func assertMCPFailureSpan(t *testing.T, spans []tracetest.SpanStub, name, token, sessionID, code string) {
	t.Helper()
	for _, span := range spans {
		// Unrelated child observations cannot satisfy the transport audit assertion.
		if span.Name != name {
			continue
		}
		attributes := make(map[string]string, len(span.Attributes))
		for _, item := range span.Attributes {
			value := item.Value.Emit()
			// Transport identity and bearer material are prohibited even on failed execution diagnostics.
			if strings.Contains(value, token) || strings.Contains(value, sessionID) {
				t.Fatalf("failure span attribute %q exposed transport identity", item.Key)
			}
			attributes[string(item.Key)] = value
		}
		want := map[string]string{"error.code": code, "mcp.request_delivery": "not_started", "mcp.session_state": "terminated", "mcp.side_effect_state": "none", "outcome": "dispatch_failed"}
		for key, expected := range want {
			// Each required enum is asserted independently so unrelated safe transport attributes remain permitted.
			if attributes[key] != expected {
				t.Fatalf("failure span attributes = %#v", attributes)
			}
		}
		return
	}
	t.Fatalf("failure span %q was not emitted", name)
}
