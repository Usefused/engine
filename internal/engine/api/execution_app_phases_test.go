package api

import (
	"context"
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/executionappvm"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"testing"
	"time"
)

// TestAppPhaseMeasurementsMatchOTELAndReceipt verifies the UI projection is durable and independent of exporter storage.
func TestAppPhaseMeasurementsMatchOTELAndReceipt(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	// Global provider changes are confined to this test's lifetime.
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	ctx, parent := provider.Tracer("test").Start(context.Background(), "runtime")
	defer parent.End()
	host := NewRecordingCapabilityHost(nil)
	start := time.Now().Add(-time.Millisecond)
	phases := []executionappvm.PhaseTiming{{Name: "compilation", StartedAt: start, Duration: time.Millisecond, Failed: true}}
	host.RecordExecutionPhases(ctx, phases)
	host.RecordExecutionPhases(ctx, phases)
	var timings map[string]float64
	// Canonical history uses the existing duration map, without a second analytics store.
	if err := json.Unmarshal(host.phaseTimings(), &timings); err != nil || timings["unified_app_compilation"] != 1 {
		t.Fatalf("incorrect durable timings: %v %v", timings, err)
	}
	spans := recorder.Ended()
	// Duplicate completion reports do not duplicate spans; both projections preserve the same real duration.
	if len(spans) != 1 || spans[0].Name() != "engine.unified_app.compilation" || spans[0].EndTime().Sub(spans[0].StartTime()) != time.Millisecond || spans[0].Parent().SpanID() != parent.SpanContext().SpanID() || spans[0].Status().Code != codes.Error {
		t.Fatalf("incorrect phase spans: %#v", spans)
	}
	invalid := NewRecordingCapabilityHost(nil)
	invalid.RecordExecutionPhases(ctx, []executionappvm.PhaseTiming{{Name: "private input sentinel", StartedAt: start}})
	// Worker-supplied labels outside the closed stage list cannot enter activity or telemetry.
	if len(invalid.phaseTimings()) != 0 || len(recorder.Ended()) != 1 {
		t.Fatal("untrusted stage was retained")
	}
}
