package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// mcpSignalingWriter exposes the exact point at which an initialize request reaches the child boundary.
type mcpSignalingWriter struct {
	bytes.Buffer
	written chan struct{}
	once    sync.Once
}

// Write records the payload before releasing the test that owns the competing termination.
func (writer *mcpSignalingWriter) Write(payload []byte) (int, error) {
	written, err := writer.Buffer.Write(payload)
	writer.once.Do(func() { close(writer.written) })
	return written, err
}

// Close satisfies the runtime stdin contract without owning an external process.
func (*mcpSignalingWriter) Close() error { return nil }

// mcpResponseFailureWriter simulates a client that disappears after response headers are committed.
type mcpResponseFailureWriter struct {
	header http.Header
	status int
}

// Header retains negotiated headers so the test can prove the abandoned session is still retired.
func (writer *mcpResponseFailureWriter) Header() http.Header { return writer.header }

// WriteHeader records the status that had already become visible at the failed delivery boundary.
func (writer *mcpResponseFailureWriter) WriteHeader(status int) { writer.status = status }

// Write rejects every response byte to model a disconnected client without buffering sensitive content.
func (*mcpResponseFailureWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// mcpSerialProbeWriter blocks the first child write so a concurrent SSE POST can attempt the same boundary.
type mcpSerialProbeWriter struct {
	active       atomic.Int32
	overlapped   atomic.Bool
	firstStarted chan struct{}
	releaseFirst chan struct{}
	once         sync.Once
	mu           sync.Mutex
	payloads     []string
}

// Write detects overlap and preserves each complete newline-delimited payload for protocol assertions.
func (writer *mcpSerialProbeWriter) Write(payload []byte) (int, error) {
	active := writer.active.Add(1)
	// More than one active call proves child framing was allowed to interleave.
	if active > 1 {
		writer.overlapped.Store(true)
	}
	blocked := false
	writer.once.Do(func() {
		blocked = true
		close(writer.firstStarted)
	})
	// Holding the first write open makes serialization observable without relying on pipe scheduling.
	if blocked {
		<-writer.releaseFirst
	}
	writer.mu.Lock()
	writer.payloads = append(writer.payloads, string(payload))
	writer.mu.Unlock()
	writer.active.Add(-1)
	return len(payload), nil
}

// Close satisfies the child writer contract without changing the probe's captured state.
func (*mcpSerialProbeWriter) Close() error { return nil }

// TestMCPStreamableInitializeCannotCommitAfterTermination prevents a stale header and ended-then-initialized history.
func TestMCPStreamableInitializeCannotCommitAfterTermination(t *testing.T) {
	input := &mcpSignalingWriter{written: make(chan struct{})}
	sess := installMCPDirectLifecycleSession(t, mcpStreamableTransport, input)
	request := mcpJSONRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("41"), Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","clientInfo":{"name":"race-client","version":"1"}}`),
	}
	response := httptest.NewRecorder()
	_, span := otel.Tracer("test").Start(context.Background(), "test.initialize_termination")
	done := make(chan bool, 1)
	go func() {
		done <- serveMCPStreamableRequest(context.Background(), span, response, sess, []byte(`{"jsonrpc":"2.0","id":41,"method":"initialize"}`), request)
	}()
	select {
	case <-input.written:
	case <-time.After(time.Second):
		t.Fatal("initialize request did not reach the child boundary")
	}
	// Termination must win before the delayed child response attempts to commit negotiated state.
	if !terminateMCPSession(sess.sessionID, "token_revoked") {
		t.Fatal("active initialize session was not terminated")
	}
	sess.responses <- `{"jsonrpc":"2.0","id":41,"result":{"protocolVersion":"2025-06-18"}}`
	accepted := <-done
	span.End()
	// A lost lifecycle commit returns no session identity that the client could incorrectly retain.
	if accepted || response.Header().Get(mcpSessionIDHeader) != "" || response.Header().Get(mcpProtocolVersionHeader) != "" {
		t.Fatalf("terminated initialize = accepted:%v headers:%v body:%s", accepted, response.Header(), response.Body.String())
	}
}

// TestMCPStreamableQueuedRequestDoesNotDispatchAfterTermination closes the authenticate-to-write race.
func TestMCPStreamableQueuedRequestDoesNotDispatchAfterTermination(t *testing.T) {
	input := &streamableWriteBuffer{}
	sess := installMCPDirectLifecycleSession(t, mcpStreamableTransport, input)
	request := mcpJSONRPCRequest{JSONRPC: "2.0", ID: json.RawMessage("42"), Method: "tools/list"}
	response := httptest.NewRecorder()
	_, span := otel.Tracer("test").Start(context.Background(), "test.queued_termination")
	sess.requestMu.Lock()
	started, done := make(chan struct{}), make(chan bool, 1)
	go func() {
		close(started)
		done <- serveMCPStreamableRequest(context.Background(), span, response, sess, []byte(`{"jsonrpc":"2.0","id":42,"method":"tools/list"}`), request)
	}()
	<-started
	// Removal happens while the request is queued, so later lock acquisition cannot authorize child delivery.
	if !terminateMCPSession(sess.sessionID, "client_terminated") {
		t.Fatal("queued request session was not terminated")
	}
	sess.requestMu.Unlock()
	accepted := <-done
	span.End()
	// No child byte may cross after the session has been retired.
	if accepted || input.Len() != 0 || response.Code != http.StatusNotFound {
		t.Fatalf("queued request = accepted:%v child-bytes:%d status:%d body:%s", accepted, input.Len(), response.Code, response.Body.String())
	}
}

// TestMCPStreamableInitializeDeliveryFailureRetiresSession prevents detached orphan runtimes.
func TestMCPStreamableInitializeDeliveryFailureRetiresSession(t *testing.T) {
	exporter := installStreamableTestTracer(t)
	sess := installMCPDirectLifecycleSession(t, mcpStreamableTransport, &streamableWriteBuffer{})
	sess.responses <- `{"jsonrpc":"2.0","id":43,"result":{"protocolVersion":"2025-06-18"}}`
	request := mcpJSONRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("43"), Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","clientInfo":{"name":"delivery-client","version":"1"}}`),
	}
	response := &mcpResponseFailureWriter{header: make(http.Header)}
	_, span := otel.Tracer("engine").Start(context.Background(), "test.initialize_delivery")
	accepted := serveMCPStreamableRequest(context.Background(), span, response, sess, []byte(`{"jsonrpc":"2.0","id":43,"method":"initialize"}`), request)
	span.End()
	_, active := lookupMCPSession(sess.sessionID)
	// Even after headers were committed, failed body delivery must leave no provider-capable orphan.
	if accepted || active || response.status != http.StatusOK {
		t.Fatalf("failed delivery = accepted:%v active:%v status:%d", accepted, active, response.status)
	}
	assertMCPResponseDeliverySpan(t, exporter.GetSpans())
}

// TestMCPStreamableInitializeWithoutClientInfoPublishesOnce separates optional attribution from protocol state.
func TestMCPStreamableInitializeWithoutClientInfoPublishesOnce(t *testing.T) {
	sess := installMCPDirectLifecycleSession(t, mcpStreamableTransport, &streamableWriteBuffer{})
	capture := installMCPDirectLifecycleCapture(t)
	sess.responses <- `{"jsonrpc":"2.0","id":44,"result":{"protocolVersion":"2025-06-18"}}`
	request := mcpJSONRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("44"), Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18"}`),
	}
	response := httptest.NewRecorder()
	_, span := otel.Tracer("test").Start(context.Background(), "test.initialize_without_client_info")
	accepted := serveMCPStreamableRequest(context.Background(), span, response, sess, []byte(`{"jsonrpc":"2.0","id":44,"method":"initialize"}`), request)
	span.End()
	// Replaying a successful child result cannot create a second initialized transition.
	if commitMCPStreamableInitialize(sess, "2025-06-18") {
		t.Fatal("duplicate initialize result committed twice")
	}
	capture.mu.Lock()
	messages := append([][]byte(nil), capture.messages...)
	capture.mu.Unlock()
	// Client attribution is optional, but one valid child result must still publish initialized exactly once.
	if !accepted || len(messages) != 1 || !bytes.Contains(messages[0], []byte(`"type":"initialized"`)) {
		t.Fatalf("initialize without clientInfo = accepted:%v events:%q body:%s", accepted, messages, response.Body.String())
	}
}

// TestMCPSSEFailedInitializeDoesNotPublishInitialized keeps child rejection distinct from request attribution.
func TestMCPSSEFailedInitializeDoesNotPublishInitialized(t *testing.T) {
	sess := installMCPDirectLifecycleSession(t, "sse", &streamableWriteBuffer{})
	capture := installMCPDirectLifecycleCapture(t)
	body := []byte(`{"jsonrpc":"2.0","id":45,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"failed-client","version":"1"}}}`)
	response := httptest.NewRecorder()
	_, span := otel.Tracer("test").Start(context.Background(), "test.sse_failed_initialize")
	dispatchMCPSSEMessage(context.Background(), span, response, sess, body)
	span.End()
	// The child error consumes the pending attempt but cannot authorize initialized state.
	handleMCPResponse(`{"jsonrpc":"2.0","id":45,"error":{"code":-32602,"message":"unsupported version"}}`, sess.sessionID)
	// A delayed success for the consumed attempt must not resurrect it.
	handleMCPResponse(`{"jsonrpc":"2.0","id":45,"result":{"protocolVersion":"2025-06-18"}}`, sess.sessionID)
	sess.lifecycleMu.Lock()
	initialized, pending := sess.initialized, sess.initializeRequestID
	sess.lifecycleMu.Unlock()
	capture.mu.Lock()
	eventCount := len(capture.messages)
	capture.mu.Unlock()
	metadata := mcpSessionMetadata(sess)
	// The bounded first claim remains available for the eventual end event without falsely recording successful negotiation.
	if response.Code != http.StatusAccepted || initialized || pending != "" || eventCount != 0 || metadata.ClientName != "failed-client" {
		t.Fatalf("failed SSE initialize = status:%d initialized:%v pending:%q events:%d metadata:%+v", response.Code, initialized, pending, eventCount, metadata)
	}
}

// TestMCPSSEConcurrentPostsSerializeChildWrites protects newline framing for large legacy messages.
func TestMCPSSEConcurrentPostsSerializeChildWrites(t *testing.T) {
	probe := &mcpSerialProbeWriter{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
	sess := installMCPDirectLifecycleSession(t, "sse", probe)
	bodies := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":51,"method":"tools/list"}`),
		[]byte(`{"jsonrpc":"2.0","id":52,"method":"tools/list"}`),
	}
	responses := []*httptest.ResponseRecorder{httptest.NewRecorder(), httptest.NewRecorder()}
	var group sync.WaitGroup
	group.Add(2)
	// The first dispatch owns the probe until the test has started its competitor.
	go func() {
		defer group.Done()
		_, span := otel.Tracer("test").Start(context.Background(), "test.sse_first")
		defer span.End()
		dispatchMCPSSEMessage(context.Background(), span, responses[0], sess, bodies[0])
	}()
	select {
	case <-probe.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first SSE write did not start")
	}
	go func() {
		defer group.Done()
		_, span := otel.Tracer("test").Start(context.Background(), "test.sse_second")
		defer span.End()
		dispatchMCPSSEMessage(context.Background(), span, responses[1], sess, bodies[1])
	}()
	// Give the competing goroutine time to expose overlap before releasing the first write.
	time.Sleep(20 * time.Millisecond)
	close(probe.releaseFirst)
	group.Wait()
	probe.mu.Lock()
	payloads := append([]string(nil), probe.payloads...)
	probe.mu.Unlock()
	// Serialization preserves two complete frames and both accepted POST acknowledgements.
	if probe.overlapped.Load() || len(payloads) != 2 || responses[0].Code != http.StatusAccepted || responses[1].Code != http.StatusAccepted {
		t.Fatalf("SSE writes = overlap:%v payloads:%q statuses:%d/%d", probe.overlapped.Load(), payloads, responses[0].Code, responses[1].Code)
	}
}

// TestMCPRuntimeFailureClassificationLimitsUncertaintyToExecute prevents read-only retries from being mislabeled as mutations.
func TestMCPRuntimeFailureClassificationLimitsUncertaintyToExecute(t *testing.T) {
	tests := []struct {
		name, method, params, code, provider string
	}{
		{name: "protocol list", method: "tools/list", code: mcpRuntimeDispatchFailedCode, provider: "not_started"},
		{name: "documentation search", method: "tools/call", params: `{"name":"search_docs"}`, code: mcpRuntimeDispatchFailedCode, provider: "not_started"},
		{name: "execute", method: "tools/call", params: `{"name":"execute"}`, code: mcpRuntimeOutcomeUnknownCode, provider: "unknown"},
	}
	for _, test := range tests {
		// Each request shape receives only the uncertainty justified by its provider capability.
		t.Run(test.name, func(t *testing.T) {
			failure := mcpDispatchFailure(mcpJSONRPCRequest{Method: test.method, Params: json.RawMessage(test.params)}, 1)
			if failure.Code != test.code || failure.ProviderExecution != test.provider || failure.AutomaticReplay {
				t.Fatalf("failure = %+v, want code:%s provider:%s", failure, test.code, test.provider)
			}
		})
	}
}

// TestMCPInitializeResultRequiresJSONRPCVersion rejects child envelopes that cannot establish MCP state.
func TestMCPInitializeResultRequiresJSONRPCVersion(t *testing.T) {
	for _, response := range []string{
		`{"id":1,"result":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"1.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`,
	} {
		// Missing or substituted protocol markers cannot authorize transport headers.
		if _, valid := mcpInitializeResultProtocolVersion(response); valid {
			t.Fatalf("accepted invalid initialize response: %s", response)
		}
	}
	// The exact JSON-RPC marker still admits a valid negotiated token.
	if version, valid := mcpInitializeResultProtocolVersion(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`); !valid || version != "2025-06-18" {
		t.Fatalf("rejected valid initialize response: %q/%v", version, valid)
	}
}

// installMCPDirectLifecycleSession registers a dependency-free session for transport-boundary tests.
func installMCPDirectLifecycleSession(t *testing.T, transport string, stdin io.WriteCloser) *mcpSession {
	t.Helper()
	previousCache, previousNATS := globalObjectCache, globalNATSClient
	globalObjectCache, globalNATSClient = nil, nil
	// Synthetic sessions own neither cache references nor durable lifecycle publication.
	t.Cleanup(func() { globalObjectCache, globalNATSClient = previousCache, previousNATS })
	sess := &mcpSession{
		appID: uuid.NewString(), sessionID: uuid.NewString(), transport: transport,
		protocolVersion: "2025-06-18", stdin: stdin, responses: make(chan string, 2),
		pendingRequests: make(map[string]struct{}), searchTelemetry: make(map[string]*mcpSearchObservation),
	}
	mcpSessions.Lock()
	mcpSessions.m[sess.sessionID] = sess
	mcpSessions.Unlock()
	// Cleanup is idempotent because individual tests may intentionally retire the session first.
	t.Cleanup(func() { terminateMCPSession(sess.sessionID, "test_cleanup") })
	return sess
}

// installMCPDirectLifecycleCapture records ordered events without leaving a publisher for cleanup transitions.
func installMCPDirectLifecycleCapture(t *testing.T) *mcpSessionLifecycleCapture {
	t.Helper()
	previous := globalNATSClient
	capture := &mcpSessionLifecycleCapture{}
	globalNATSClient = &messaging.NATSClient{JS: capture}
	// Restore before the session helper runs its later cleanup and emits an unrelated ended transition.
	t.Cleanup(func() { globalNATSClient = previous })
	return capture
}

// assertMCPResponseDeliverySpan verifies bounded auditing without relying on raw transport errors.
func assertMCPResponseDeliverySpan(t *testing.T, spans []tracetest.SpanStub) {
	t.Helper()
	for _, span := range spans {
		// Only the direct delivery span owns the expected failure outcome.
		if span.Name != "test.initialize_delivery" {
			continue
		}
		attributes := map[string]string{}
		for _, item := range span.Attributes {
			attributes[string(item.Key)] = item.Value.Emit()
		}
		// Initialization cannot execute a provider even when the HTTP body is lost.
		if attributes["outcome"] != "response_delivery_failed" || attributes["error.code"] != mcpRuntimeResponseFailedCode || attributes["mcp.side_effect_state"] != "none" {
			t.Fatalf("delivery span attributes = %#v", attributes)
		}
		return
	}
	t.Fatal("response delivery failure span was not emitted")
}
