package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestUnifiedAppRunTracePreservesParentWithoutPayloads exercises tracing even when execution admission fails.
func TestUnifiedAppRunTracePreservesParentWithoutPayloads(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	// Restore process-wide telemetry so neighboring tests retain their own tracing configuration.
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	ctx, parent := provider.Tracer("test").Start(context.Background(), "caller")
	identity := auth.RuntimeIdentity{AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(), TokenID: uuid.New()}
	_, requestErr := (&EngineGRPCServer{}).executeCapabilityRun(ctx, capabilityRunSpec{identity: identity, input: json.RawMessage(`{"private":"request sentinel"}`)})
	parent.End()
	// Missing storage intentionally rejects execution before code or provider effects can run.
	if requestErr == nil {
		t.Fatal("expected admission failure")
	}
	spans := recorder.Ended()
	// The shared run span must remain a child of the caller and carry an error status.
	if len(spans) != 2 || spans[0].Name() != "engine.unified_app.run" || spans[0].Parent().SpanID() != parent.SpanContext().SpanID() || spans[0].Status().Code != codes.Error {
		t.Fatalf("unexpected spans: %#v", spans)
	}
	raw, _ := json.Marshal(tracetest.SpanStubsFromReadOnlySpans(spans))
	// Neither input nor runtime token identity is an exporter dimension.
	if strings.Contains(string(raw), "request sentinel") || strings.Contains(string(raw), identity.TokenID.String()) {
		t.Fatal("private execution data entered telemetry")
	}
}

// TestUnifiedAppReceiptTraceCorrelation links durable history to the caller's configured trace without inventing IDs.
func TestUnifiedAppReceiptTraceCorrelation(t *testing.T) {
	provider := sdktrace.NewTracerProvider()
	defer provider.Shutdown(context.Background())
	ctx, span := provider.Tracer("test").Start(context.Background(), "run")
	defer span.End()
	now := time.Now()
	record := &store.ExecutionResult{CreatedAt: now, CompletedAt: &now, Status: "failed"}
	event := unifiedAppReceipt(ctx, record, capabilityRunSpec{})
	// Parent and provider spans can now be found from the same durable request receipt.
	if event.TraceID != span.SpanContext().TraceID().String() || event.SpanID != span.SpanContext().SpanID().String() {
		t.Fatal("receipt lost trace correlation")
	}
	event = unifiedAppReceipt(context.Background(), record, capabilityRunSpec{})
	// An Engine with tracing disabled must not expose misleading zero trace identities.
	if event.TraceID != "" || event.SpanID != "" {
		t.Fatal("fabricated trace identity")
	}
	// Author-controlled phase data cannot smuggle private text into span attributes.
	if telemetryFailurePhase("private phase sentinel") != "runtime" || telemetryFailurePhase("output_validation") != "output_validation" {
		t.Fatal("phase allowlist failed")
	}
}
