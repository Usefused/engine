package sandbox

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestMCPSearchTelemetryAllowlistAndDenylist proves successful agent discovery emits only fixed metadata.
func TestMCPSearchTelemetryAllowlistAndDenylist(t *testing.T) {
	exporter := installStreamableTestTracer(t)
	fixture := &Fixture{
		Operations:        []FixtureOperation{{OperationID: "secret.operation", Description: "secret description"}},
		UnifiedOperations: &models.SDKUnifiedOperationDescriptors{Operations: []models.SDKUnifiedOperationDescriptor{{Name: "secret.unified", InputSchema: json.RawMessage(`{"secret_schema":true}`)}}},
	}
	sess := &mcpSession{transport: mcpStreamableTransport, fixture: fixture, pendingRequests: make(map[string]struct{}), sessionID: "secret-session", token: "secret-token"}
	request := mcpJSONRPCRequest{Method: "tools/call", ID: json.RawMessage(`7`), Params: json.RawMessage(`{"name":"search_docs","arguments":{"query":"secret query intent"}}`)}
	callID := trackMCPToolCall(context.Background(), request, sess)
	response := `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"secret result /properties/password Authorization Bearer secret-credential provider-body"}]}}`
	completeMCPToolCall(sess, callID, response, "")

	span := recordedMCPSearchSpan(t, exporter.GetSpans())
	attributes := mcpSearchTestAttributes(span)
	assertMCPSearchTelemetryAttributes(t, attributes, len(response))
	joined := strings.Join(mcpSearchTestValues(attributes), " ")
	// Request, catalogue, schema, response, and routing sentinels must never survive projection.
	for _, denied := range []string{"secret query intent", "secret.operation", "secret.unified", "secret description", "secret_schema", "/properties/password", "secret result", "secret-session", "secret-token", "secret-credential", "provider-body"} {
		// A sentinel match proves caller or provider material escaped the projection.
		if strings.Contains(joined, denied) {
			t.Fatalf("search telemetry exposed %q in %q", denied, joined)
		}
	}
}

// assertMCPSearchTelemetryAttributes keeps exact metadata checks separate from the content denylist.
func assertMCPSearchTelemetryAttributes(t *testing.T, attributes map[string]string, responseBytes int) {
	t.Helper()
	allowed := map[string]struct{}{
		"actor.type": {}, "mcp.transport": {}, "mcp.search.mode": {},
		"mcp.search.physical_count": {}, "mcp.search.unified_count": {},
		"mcp.search.include_detail": {}, "mcp.search.response_bytes": {},
		"mcp.search.duration_ms": {}, "outcome": {},
	}
	// Exact allowlisting forces any future dimension through a deliberate privacy review.
	for key := range attributes {
		// Any new key can create cardinality or disclosure risk and must fail closed here.
		if _, ok := allowed[key]; !ok {
			t.Fatalf("unexpected search attribute %q", key)
		}
	}
	// Every approved dimension must be present, including the dynamic duration measurement.
	if len(attributes) != len(allowed) {
		t.Fatalf("search attributes = %#v", attributes)
	}
	expected := map[string]string{
		"actor.type": "agent", "mcp.search.mode": "query", "mcp.search.include_detail": "true",
		"outcome": "succeeded", "mcp.search.physical_count": "1", "mcp.search.unified_count": "1",
		"mcp.search.response_bytes": strconv.Itoa(responseBytes),
	}
	for key, value := range expected {
		// Fixed identity and bounded aggregates must match without inspecting tool content.
		if attributes[key] != value {
			t.Fatalf("search attribute %q = %q, want %q", key, attributes[key], value)
		}
	}
}

// TestMCPSearchTelemetryFailureUsesStableCode proves raw JSON-RPC failures stay outside the span.
func TestMCPSearchTelemetryFailureUsesStableCode(t *testing.T) {
	exporter := installStreamableTestTracer(t)
	sess := &mcpSession{transport: "sse", fixture: &Fixture{}, pendingRequests: make(map[string]struct{})}
	request := mcpJSONRPCRequest{Method: "tools/call", ID: json.RawMessage(`"call"`), Params: json.RawMessage(`{"name":"search_docs","arguments":{"operationId":"secret.operation","section":"secret.pointer"}}`)}
	callID := trackMCPToolCall(context.Background(), request, sess)
	completeMCPToolCall(sess, callID, `{"jsonrpc":"2.0","id":"call","error":{"code":-32603,"message":"raw secret error"}}`, "")

	span := recordedMCPSearchSpan(t, exporter.GetSpans())
	attributes := mcpSearchTestAttributes(span)
	allowed := map[string]struct{}{
		"actor.type": {}, "mcp.transport": {}, "mcp.search.mode": {},
		"mcp.search.physical_count": {}, "mcp.search.unified_count": {},
		"mcp.search.include_detail": {}, "mcp.search.response_bytes": {},
		"mcp.search.duration_ms": {}, "outcome": {}, "error.code": {},
	}
	// Failure telemetry adds exactly one stable code to the success allowlist.
	for key := range attributes {
		// Failure must not introduce message, argument, or result dimensions.
		if _, ok := allowed[key]; !ok {
			t.Fatalf("unexpected failure attribute %q", key)
		}
	}
	// Section failures retain only their closed mode, outcome, and classifier.
	if len(attributes) != len(allowed) || attributes["mcp.search.mode"] != "section" || attributes["error.code"] != "jsonrpc_error" || attributes["outcome"] != "failed" {
		t.Fatalf("failure attributes = %#v", attributes)
	}
	joined := strings.Join(mcpSearchTestValues(attributes), " ")
	// Neither exact lookup values nor child error text may enter the stable failure span.
	for _, denied := range []string{"secret.operation", "secret.pointer", "raw secret error"} {
		// Raw lookup and error sentinels are prohibited even on failed calls.
		if strings.Contains(joined, denied) {
			t.Fatalf("failure telemetry exposed %q in %q", denied, joined)
		}
	}
}

// TestMCPSearchModeUsesClosedRuntimeVocabulary locks request classification to four bounded values.
func TestMCPSearchModeUsesClosedRuntimeVocabulary(t *testing.T) {
	tests := []struct {
		params string
		mode   string
	}{
		{params: `{"name":"search_docs","arguments":{}}`, mode: "list"},
		{params: `{"name":"search_docs","arguments":{"query":"intent"}}`, mode: "query"},
		{params: `{"name":"search_docs","arguments":{"operationId":"operation"}}`, mode: "operation"},
		{params: `{"name":"search_docs","arguments":{"operationId":"operation","section":"request"}}`, mode: "section"},
		{params: `{"name":"search_docs","arguments":{"schemaPath":"/properties/id"}}`, mode: "section"},
	}
	// Every public discovery shape must collapse to one reviewed mode value.
	for _, test := range tests {
		mode, ok := mcpSearchMode(json.RawMessage(test.params))
		// A valid search_docs request must be recognized without retaining its values.
		if !ok || mode != test.mode {
			t.Fatalf("mcpSearchMode(%s) = %q/%t, want %q/true", test.params, mode, ok, test.mode)
		}
	}
}

// TestMCPExecuteDoesNotCreateSearchTelemetry keeps the new path scoped to documentation discovery.
func TestMCPExecuteDoesNotCreateSearchTelemetry(t *testing.T) {
	exporter := installStreamableTestTracer(t)
	sess := &mcpSession{transport: "sse", fixture: &Fixture{}, pendingRequests: make(map[string]struct{})}
	request := mcpJSONRPCRequest{Method: "tools/call", ID: json.RawMessage(`9`), Params: json.RawMessage(`{"name":"execute","arguments":{"script":"return 'secret'"}}`)}
	callID := trackMCPToolCall(context.Background(), request, sess)
	completeMCPToolCall(sess, callID, `{"jsonrpc":"2.0","id":9,"result":{}}`, "")
	// Execute retains its existing execution telemetry and must not create a documentation span.
	if len(exporter.GetSpans()) != 0 {
		t.Fatalf("non-search spans = %d, want 0", len(exporter.GetSpans()))
	}
}

// recordedMCPSearchSpan returns the one canonical documentation span emitted by a test call.
func recordedMCPSearchSpan(t *testing.T, spans []tracetest.SpanStub) tracetest.SpanStub {
	t.Helper()
	// One search request must produce exactly one ended span.
	if len(spans) != 1 || spans[0].Name != "engine.sandbox.mcp.search_docs" {
		t.Fatalf("search spans = %#v", spans)
	}
	return spans[0]
}

// mcpSearchTestAttributes projects recorded OTEL values for exact allowlist assertions.
func mcpSearchTestAttributes(span tracetest.SpanStub) map[string]string {
	attributes := make(map[string]string, len(span.Attributes))
	// Test projection preserves every recorded value for exact comparison.
	for _, item := range span.Attributes {
		attributes[string(item.Key)] = item.Value.Emit()
	}
	return attributes
}

// mcpSearchTestValues returns attribute values without depending on map iteration order.
func mcpSearchTestValues(attributes map[string]string) []string {
	values := make([]string, 0, len(attributes))
	// Ordering is irrelevant because denylist checks scan the joined set.
	for _, value := range attributes {
		values = append(values, value)
	}
	return values
}
