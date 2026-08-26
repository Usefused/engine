package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/executionevent"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/metadata"
)

type receiptTimingCapture struct {
	captureJetStreamPublisher
	count int
}

// PublishMsgJS counts canonical publications so timing repair cannot add duplicate receipts.
func (capture *receiptTimingCapture) PublishMsgJS(message *nats.Msg) (*nats.PubAck, error) {
	capture.count++
	return capture.captureJetStreamPublisher.PublishMsgJS(message)
}

// captureReceiptTimings observes the existing event publisher without adding a persistence path.
func captureReceiptTimings(t *testing.T) *receiptTimingCapture {
	t.Helper()
	capture := &receiptTimingCapture{}
	executionevent.SetPublisher(executionevent.NewPublisher(capture))
	// Package fixtures restore process-owned publishers after each serial case.
	t.Cleanup(func() { executionevent.SetPublisher(nil) })
	return capture
}

// TestMCPPhysicalReceiptTimings covers the actual bridge with and without OTEL export configured.
func TestMCPPhysicalReceiptTimings(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		tracing bool
	}{
		{name: "success", status: http.StatusOK, tracing: true},
		{name: "provider failure", status: http.StatusServiceUnavailable, tracing: true},
		{name: "no exporter", status: http.StatusOK},
	} {
		// Every case owns its runtime globals and a synthetic provider; no connected account is used.
		t.Run(test.name, func(t *testing.T) {
			recorder, _ := latencyEvidenceTracer(t)
			// Receipt timings must not depend on installing an OTEL exporter.
			if !test.tracing {
				otel.SetTracerProvider(noop.NewTracerProvider())
			}
			vendor := receiptTimingProvider(t, test.status)
			sessionID, endpoint, _ := configureMCPPhysicalCallTest(t, vendor.URL)
			capture := captureReceiptTimings(t)
			body, _ := json.Marshal(mcpCallRequest{OperationID: endpoint, Params: json.RawMessage(`{}`)})
			request := httptest.NewRequest(http.MethodPost, "/mcp/call", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+sessionID)
			mcpCallHandler(httptest.NewRecorder(), request)
			event, _ := assertReceiptTimings(t, capture, models.EngineExecutionTransportMCP)
			// HTTP failures still own their provider timings and canonical failure classification.
			if event.ProviderHTTPStatus == nil || *event.ProviderHTTPStatus != test.status {
				t.Fatal("receipt lost the provider status")
			}
			// Configured tracing must correlate the durable receipt with the physical execution span.
			if test.tracing {
				span := recordedLatencySpan(t, recorder.Ended(), "engine.dispatch.execute")
				// A receipt must identify its own physical span rather than an ingress or publisher span.
				if event.TraceID != span.SpanContext().TraceID().String() || event.SpanID != span.SpanContext().SpanID().String() {
					t.Fatal("receipt lost physical trace correlation")
				}
				recordedTiming(t, span, "provider_total")
				assertLatencySpansAreSecretSafe(t, recorder.Ended(), "server-side-token", "receipt-timing-private-fixture")
			}
		})
	}
}

// receiptTimingProvider sends a local synthetic payload that must never enter receipt or trace metadata.
func receiptTimingProvider(t *testing.T, status int) *httptest.Server {
	t.Helper()
	// A small fixed delay distinguishes recorded provider time from an absent or fabricated zero.
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"marker":"receipt-timing-private-fixture"}`))
	}))
	t.Cleanup(vendor.Close)
	return vendor
}

// assertReceiptTimings checks the durable event rather than inferring audit completeness from successful dispatch.
func assertReceiptTimings(t *testing.T, capture *receiptTimingCapture, transport string) (models.EngineExecutionEvent, map[string]float64) {
	t.Helper()
	// Exactly one publication remains the canonical history contract for one physical execution.
	if capture.count != 1 || capture.message == nil {
		t.Fatalf("receipt publications = %d, want one", capture.count)
	}
	var envelope models.EngineExecutionEventEnvelope
	// Exercise the serialized JetStream boundary, not just the producer's in-memory struct.
	if err := json.Unmarshal(capture.message.Data, &envelope); err != nil {
		t.Fatal(err)
	}
	event := envelope.Event
	var timings map[string]float64
	// Absent timing JSON is the original MCP regression, even when total duration is populated.
	if err := json.Unmarshal(event.Timings, &timings); err != nil {
		t.Fatalf("receipt timing map is absent or invalid: %v", err)
	}
	for _, name := range []string{"provider_total", "provider_time_to_headers", "credentials_resolution"} {
		// Zero is valid for very fast stages, but missing stages must not masquerade as measured work.
		if duration, ok := timings[name]; !ok || duration < 0 {
			t.Errorf("receipt timing %q missing or negative", name)
		}
	}
	assertReceiptTimingMetadata(t, capture.message.Data, event, timings, transport)
	return event, timings
}

// assertReceiptTimingMetadata keeps duration consistency, attribution, and privacy checks independently readable.
func assertReceiptTimingMetadata(t *testing.T, encoded []byte, event models.EngineExecutionEvent, timings map[string]float64, transport string) {
	t.Helper()
	// Provider duration must match the published timing snapshot and fit the total receipt duration.
	if event.ProviderLatencyMs == nil || *event.ProviderLatencyMs != int64(timings["provider_total"]) || *event.ProviderLatencyMs > event.LatencyMs {
		t.Error("receipt provider latency does not match its timing snapshot/total")
	}
	// Transport attribution remains unchanged while collection moves to a shared boundary.
	if event.Transport != transport {
		t.Errorf("receipt transport = %q, want %q", event.Transport, transport)
	}
	// Receipts must not acquire credentials or result data as a side effect of instrumentation.
	if bytes.Contains(encoded, []byte("receipt-timing-private-fixture")) || bytes.Contains(encoded, []byte("server-side-token")) {
		t.Error("receipt contains synthetic private material")
	}
}

// TestSDKReceiptPreservesIngressTimings proves the shared boundary reuses the gRPC collector.
func TestSDKReceiptPreservesIngressTimings(t *testing.T) {
	vendor := receiptTimingProvider(t, http.StatusOK)
	sessionID, endpoint, _ := configureMCPPhysicalCallTest(t, vendor.URL)
	capture := captureReceiptTimings(t)
	session, _ := lookupMCPSession(sessionID)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-app-id", session.appID, "x-api-key", session.token))
	stream := &fakeExecuteStream{ctx: ctx}
	// The real SDK ingress adds request_decode before the shared execution boundary runs.
	if err := NewEngineGRPCServer().Execute(&enginev1.ExecuteRequest{EndpointName: endpoint, Params: []byte(`{}`)}, stream); err != nil {
		t.Fatal(err)
	}
	_, timings := assertReceiptTimings(t, capture, models.EngineExecutionTransportSDK)
	// Replacing the SDK's collector would silently discard this ingress measurement.
	if _, ok := timings["request_decode"]; !ok {
		t.Fatal("shared execution discarded SDK ingress timings")
	}
}

// TestUnifiedChildReceiptTimingsStayIsolated prevents sibling physical calls from inheriting parent totals.
func TestUnifiedChildReceiptTimingsStayIsolated(t *testing.T) {
	vendor := receiptTimingProvider(t, http.StatusOK)
	configureMCPPhysicalCallTest(t, vendor.URL)
	identity, operation := physicalExecutionTestOperation(vendor.URL)
	operation.match.endpoint.Responses = fusedobject.Responses{"200": {Representations: []fusedobject.ResponseRepresentation{{MediaType: "application/json"}}}}
	parent := engine.NewExecutionTimings()
	parent.Record("provider_total", time.Hour)
	ctx := engine.ContextWithExecutionTimings(context.Background(), parent)
	for range 2 {
		capture := captureReceiptTimings(t)
		// Each resolved child executes through the same physical boundary used by Unified scheduling.
		if _, err := ExecuteResolvedPhysicalJSON(ctx, globalDispatcher, identity, operation, PhysicalExecutionRequest{Transport: models.EngineExecutionTransportMCP}); err != nil {
			t.Fatal(err)
		}
		assertReceiptTimings(t, capture, models.EngineExecutionTransportMCP)
	}
	// Parent timings remain untouched even after multiple physical descendants complete.
	if got := parent.SnapshotMilliseconds()["provider_total"]; got != float64(time.Hour.Milliseconds()) {
		t.Fatal("child execution mutated its parent's collector")
	}
}
