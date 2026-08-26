package sandbox

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/mcpsession"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestMCPClientIPTrustBoundary prevents forged forwarding from becoming durable client identity.
func TestMCPClientIPTrustBoundary(t *testing.T) {
	cases := []struct{ name, remote, forwarded, trusted, want string }{
		{"direct IPv4", "192.0.2.2:1234", "198.51.100.9", "", "192.0.2.2"},
		{"direct IPv6", "[2001:db8::2]:1234", "198.51.100.9", "", "2001:db8::2"},
		{"mapped IPv4", "[::ffff:192.0.2.2]:1234", "", "", "192.0.2.2"},
		{"trusted proxy", "10.0.0.1:1234", "198.51.100.9", "10.0.0.0/8", "198.51.100.9"},
		{"rightmost untrusted", "10.0.0.1:1234", "192.0.2.8, 198.51.100.9, 10.0.0.2", "10.0.0.0/8", "198.51.100.9"},
		{"IPv6 forwarding", "[fd00::1]:1234", "2001:db8::9", "fd00::/8", "2001:db8::9"},
		{"untrusted peer", "192.0.2.2:1234", "198.51.100.9", "10.0.0.0/8", "192.0.2.2"},
		{"invalid config", "10.0.0.1:1234", "198.51.100.9", "10.0.0.0/8,bad", "10.0.0.1"},
		{"invalid chain", "10.0.0.1:1234", "bad", "10.0.0.0/8", "10.0.0.1"},
		{"missing chain", "10.0.0.1:1234", "", "10.0.0.0/8", "10.0.0.1"},
		{"bounded chain", "10.0.0.1:1234", strings.Repeat("10.0.0.2,", 33), "10.0.0.0/8", "10.0.0.1"},
		{"invalid peer", "not-an-address", "198.51.100.9", "10.0.0.0/8", ""},
	}
	for _, test := range cases {
		// Each synthetic network shape has one explicit trusted-peer expectation.
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/mcp/test", nil)
			request.RemoteAddr = test.remote
			request.Header.Set("X-Forwarded-For", test.forwarded)
			// Header values must never override an untrusted transport hop.
			if got := mcpClientIP(request, test.trusted); got != test.want {
				t.Fatalf("IP = %q, want %q", got, test.want)
			}
		})
	}
}

type mcpSessionLifecycleCapture struct {
	nats.JetStreamContext
	mu       sync.Mutex
	messages [][]byte
}

// Publish captures the canonical lifecycle wire without starting an external message server.
func (capture *mcpSessionLifecycleCapture) Publish(_ string, data []byte, _ ...nats.PubOpt) (*nats.PubAck, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.messages = append(capture.messages, append([]byte(nil), data...))
	return &nats.PubAck{}, nil
}

// TestMCPSessionMetadataLifecycleWire proves SSE enrichment and termination remain one credential-free lifecycle path.
func TestMCPSessionMetadataLifecycleWire(t *testing.T) {
	capture := &mcpSessionLifecycleCapture{}
	previous := globalNATSClient
	globalNATSClient = &messaging.NATSClient{JS: capture}
	// Tests must restore the shared publisher so parallel package execution cannot inherit the fixture.
	t.Cleanup(func() { globalNATSClient = previous })
	session := &mcpSession{appID: "synthetic-app", sessionID: "synthetic-session", protocolVersion: "2025-06-18", token: "never-publish-token", clientMetadata: mcpsession.Metadata{InitialClientIP: "192.0.2.2"}}
	publishMCPSessionEvent(session, "started", "")
	captureMCPClientInfo(context.Background(), mcpJSONRPCRequest{Method: "initialize", ID: []byte("1"), Params: []byte(`{"clientInfo":{"name":"Example Agent","version":"1"}}`)}, session)
	publishMCPSessionEvent(session, "ended", "client_terminated")
	// A metadata transition enriches the session exactly once between start and end.
	if len(capture.messages) != 3 {
		t.Fatalf("lifecycle messages = %d", len(capture.messages))
	}
	for index, expected := range []string{"started", "initialized", "ended"} {
		var payload map[string]any
		// Every published transition must remain a single structured document.
		if json.Unmarshal(capture.messages[index], &payload) != nil {
			t.Fatal("invalid lifecycle JSON")
		}
		// Network provenance belongs in history, while runtime credentials never cross the wire.
		if payload["type"] != expected || payload["initial_client_ip"] != "192.0.2.2" || strings.Contains(string(capture.messages[index]), session.token) {
			t.Fatal("unsafe or incomplete lifecycle projection")
		}
	}
}

// TestCaptureMCPClientInfoIsBoundedAndFirstClaimWins covers both adapters' shared initialization boundary.
func TestCaptureMCPClientInfoIsBoundedAndFirstClaimWins(t *testing.T) {
	session := &mcpSession{clientMetadata: mcpsession.Metadata{InitialClientIP: "192.0.2.2"}}
	captureMCPClientInfo(context.Background(), mcpJSONRPCRequest{Method: "tools/call", Params: []byte(`{"clientInfo":{"name":"ignored"}}`)}, session)
	captureMCPClientInfo(context.Background(), mcpJSONRPCRequest{Method: "initialize", ID: []byte("1"), Params: []byte(`{"clientInfo":{"name":"bad\nname"}}`)}, session)
	// Malformed claims cannot lock out the first valid initialization.
	if session.clientInfoRecorded {
		t.Fatal("invalid initialization was retained")
	}
	captureMCPClientInfo(context.Background(), mcpJSONRPCRequest{Method: "initialize", ID: []byte("1"), Params: []byte(`{"clientInfo":{"name":"Example Agent","version":"1.2.3"}}`)}, session)
	captureMCPClientInfo(context.Background(), mcpJSONRPCRequest{Method: "initialize", ID: []byte("1"), Params: []byte(`{"clientInfo":{"name":"Replacement"}}`)}, session)
	metadata := mcpSessionMetadata(session)
	// Initial transport provenance and first client claim remain immutable across subsequent requests.
	if metadata != (mcpsession.Metadata{ClientName: "Example Agent", ClientVersion: "1.2.3", InitialClientIP: "192.0.2.2"}) {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
}

// TestCaptureMCPClientInfoConcurrentLifecycleReads exercises the lock shared by initialization and end publication.
func TestCaptureMCPClientInfoConcurrentLifecycleReads(t *testing.T) {
	session := &mcpSession{}
	request := mcpJSONRPCRequest{Method: "initialize", ID: []byte("1"), Params: json.RawMessage(`{"clientInfo":{"name":"Example Agent","version":"1"}}`)}
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		// Lifecycle snapshots may race concurrent rejected/repeated initialization requests.
		go func() {
			defer group.Done()
			captureMCPClientInfo(context.Background(), request, session)
			_ = mcpSessionMetadata(session)
		}()
	}
	group.Wait()
	// Every concurrent observer must converge on the same admitted first claim.
	if mcpSessionMetadata(session).ClientName != "Example Agent" {
		t.Fatal("client claim missing")
	}
}

// TestMCPClientMetadataTelemetryContainsOnlyOutcome keeps private display and network provenance out of OTEL.
func TestMCPClientMetadataTelemetryContainsOnlyOutcome(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	// The test-owned exporter must not affect unrelated execution telemetry tests.
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	session := &mcpSession{clientMetadata: mcpsession.Metadata{InitialClientIP: "192.0.2.2"}}
	request := mcpJSONRPCRequest{Method: "initialize", ID: []byte("1"), Params: []byte(`{"clientInfo":{"name":"private-client-name","version":"private-client-version"}}`)}
	captureMCPClientInfo(context.Background(), request, session)
	captureMCPClientInfo(context.Background(), request, session)
	spans := recorder.Ended()
	// Initialization and repeated claims are independently diagnosable without tracing their values.
	if len(spans) != 2 {
		t.Fatalf("metadata spans = %d", len(spans))
	}
	for index, outcome := range []string{"recorded", "already_recorded"} {
		attributes := spans[index].Attributes()
		// An exact allowlist is stronger than searching for a few known secret strings.
		if len(attributes) != 1 || string(attributes[0].Key) != "outcome" || attributes[0].Value.AsString() != outcome {
			t.Fatalf("unexpected metadata telemetry: %#v", attributes)
		}
	}
}

// TestMCPEmptyInitializationCannotConsumeClientClaim preserves the first real handshake after malformed SSE messages.
func TestMCPEmptyInitializationCannotConsumeClientClaim(t *testing.T) {
	for _, request := range []mcpJSONRPCRequest{
		{Method: "initialize", Params: []byte(`{"clientInfo":{"name":"Notification"}}`)},
		{Method: "initialize", ID: []byte("null"), Params: []byte(`{"clientInfo":{"name":"Null identity"}}`)},
		{Method: "initialize", ID: []byte("1"), Params: []byte(`{}`)},
		{Method: "initialize", ID: []byte("1"), Params: []byte(`{"clientInfo":null}`)},
		{Method: "initialize", ID: []byte("1"), Params: []byte(`{"clientInfo":{}}`)},
		{Method: "initialize", ID: []byte("1"), Params: []byte(`{"clientInfo":{"name":"  "}}`)},
	} {
		session := &mcpSession{}
		captureMCPClientInfo(context.Background(), request, session)
		// Incomplete protocol claims must not reserve immutable provenance for the session.
		if session.clientInfoRecorded {
			t.Fatal("incomplete initialization consumed the client claim")
		}
		captureMCPClientInfo(context.Background(), mcpJSONRPCRequest{Method: "initialize", ID: []byte("2"), Params: []byte(`{"clientInfo":{"name":"Example Agent","version":"1"}}`)}, session)
		// The subsequent valid request remains discoverable rather than permanently showing Not recorded.
		if mcpSessionMetadata(session).ClientName != "Example Agent" {
			t.Fatal("real initialization could not fill client metadata")
		}
	}
}

// TestMCPSessionConcurrentPublicationChronology rejects impossible lifecycle snapshots during active client traffic.
func TestMCPSessionConcurrentPublicationChronology(t *testing.T) {
	capture := &mcpSessionLifecycleCapture{}
	previous := globalNATSClient
	globalNATSClient = &messaging.NATSClient{JS: capture}
	// Publisher ownership stays local to this synthetic test.
	t.Cleanup(func() { globalNATSClient = previous })
	session := &mcpSession{appID: "synthetic-app", sessionID: "synthetic-session"}
	ready, done := make(chan struct{}), make(chan struct{})
	// Concurrent traffic changes activity while lifecycle publication takes its independent metadata locks.
	go func() {
		close(ready)
		defer close(done)
		for range 1000 {
			session.activityMu.Lock()
			session.lastActivityAt = time.Now()
			session.activityMu.Unlock()
		}
	}()
	<-ready
	for range 1000 {
		publishMCPSessionEvent(session, "initialized", "")
	}
	<-done
	for _, raw := range capture.messages {
		var event struct {
			Timestamp      time.Time `json:"timestamp"`
			LastActivityAt time.Time `json:"last_activity_at"`
		}
		// Verify serialized timestamps, the exact values admitted by the canonical worker.
		if json.Unmarshal(raw, &event) != nil {
			t.Fatal("invalid lifecycle document")
		}
		// Later traffic may update the live session but cannot enter an earlier event snapshot.
		if event.LastActivityAt.After(event.Timestamp) {
			t.Fatal("lifecycle event would be discarded for impossible chronology")
		}
	}
}
