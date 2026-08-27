package sandbox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

// mcpSSEReadFailure returns a stable transport read error without retaining caller bytes.
type mcpSSEReadFailure struct{}

// Read fails before producing bytes so the handler exercises its pre-dispatch error contract.
func (mcpSSEReadFailure) Read([]byte) (int, error) { return 0, errors.New("synthetic read failure") }

// TestMCPSSEDispatchFailureReachesEventStream proves execute uncertainty is correlated on both legacy HTTP channels.
func TestMCPSSEDispatchFailureReachesEventStream(t *testing.T) {
	sess := installMCPDirectLifecycleSession(t, "sse", &streamableFailingWriter{written: 1})
	streamCtx, cancelStream := context.WithCancel(context.Background())
	sess.lifecycleCtx, sess.cancel = streamCtx, cancelStream
	sess.sseFailures = make(chan mcpSSEFailure, maxMCPSSEFailureQueue)
	reader, writer := io.Pipe()
	t.Cleanup(func() { cancelStream(); _ = writer.Close() })
	stream := httptest.NewRecorder()
	streamDone := make(chan struct{})
	// The real stream pump owns delivery so the test covers queueing, framing, and terminal cleanup together.
	go func() {
		processMCPStream(streamCtx, stream, stream, reader, sess.sessionID)
		close(streamDone)
	}()

	post := httptest.NewRecorder()
	request := []byte(`{"jsonrpc":"2.0","id":71,"method":"tools/call","params":{"name":"execute","arguments":{"script":"return 1"}}}`)
	_, span := otel.Tracer("test").Start(context.Background(), "test.sse_dispatch_delivery")
	dispatchMCPSSEMessage(context.Background(), span, post, sess, request)
	span.End()
	select {
	case <-streamDone:
	case <-time.After(time.Second):
		t.Fatal("server-owned SSE failure was not delivered")
	}
	for name, body := range map[string]string{"POST": post.Body.String(), "SSE": stream.Body.String()} {
		// Both channels must preserve correlation and the non-replayable execute classification.
		if !strings.Contains(body, `"id":71`) || !strings.Contains(body, mcpRuntimeOutcomeUnknownCode) || !strings.Contains(body, `"execute_request":"do_not_replay"`) || !strings.Contains(body, `"provider_execution":"unknown"`) {
			t.Fatalf("%s failure contract = %s", name, body)
		}
	}
}

// TestMCPMessageBodyReadFailureUsesStableCode keeps transport implementation errors out of public responses.
func TestMCPMessageBodyReadFailureUsesStableCode(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/mcp/message", mcpSSEReadFailure{})
	response := httptest.NewRecorder()
	_, err := readBoundedMCPMessageBody(response, request)
	// The stable code is actionable for clients while the raw reader error remains server-side only.
	if err == nil || response.Code != http.StatusBadRequest || response.Body.String() != `{"error":"mcp_message_body_read_failed"}`+"\n" {
		t.Fatalf("body read failure = %v %d/%s", err, response.Code, response.Body.String())
	}
}
