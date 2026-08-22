package sandbox

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
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

func (*streamableWriteBuffer) Close() error { return nil }

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

func TestMCPStreamableFirstPostRequiresInitialize(t *testing.T) {
	appID := uuid.NewString()
	router := streamableTestRouter()
	request := httptest.NewRequest(http.MethodPost, "/mcp/"+appID, bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte("initialize is required")) {
		t.Fatalf("status/body = %d/%s, want initialize error", response.Code, response.Body.String())
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
