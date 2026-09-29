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

// TestAppPhaseMeasurementsMatchOTELAndReceipt verifies ordinary receipts retain safe failure evidence independently of the exporter.
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
	// The failed-stage label is useful to ordinary readers but contains no authored error material.
	if host.receiptFailureReason() != "unified_app_compilation_failed" {
		t.Fatal("failed phase missing from ordinary receipt")
	}
	spans := recorder.Ended()
	// Duplicate completion reports do not duplicate spans; both projections preserve the same real duration.
	if len(spans) != 1 {
		t.Fatalf("expected one phase span, got %d", len(spans))
	}
	assertFailedCompilationSpan(t, spans[0], parent.SpanContext().SpanID().String())
	invalid := NewRecordingCapabilityHost(nil)
	invalid.RecordExecutionPhases(ctx, []executionappvm.PhaseTiming{{Name: "private input sentinel", StartedAt: start}})
	// Worker-supplied labels outside the closed stage list cannot enter activity or telemetry.
	if len(invalid.phaseTimings()) != 0 || invalid.receiptFailureReason() != "" || len(recorder.Ended()) != 1 {
		t.Fatal("untrusted stage was retained")
	}
}

// assertFailedCompilationSpan checks that the observed worker phase keeps its duration, failure and parent trace.
func assertFailedCompilationSpan(t *testing.T, span sdktrace.ReadOnlySpan, parentID string) {
	t.Helper()
	// All fields come from the one recorded worker phase, rather than synthesized UI evidence.
	if span.Name() != "engine.unified_app.compilation" || span.EndTime().Sub(span.StartTime()) != time.Millisecond || span.Parent().SpanID().String() != parentID || span.Status().Code != codes.Error {
		t.Fatalf("incorrect phase span: %#v", span)
	}
}

// TestAppPhaseFailureRequiresEvidence prevents partial timing from being interpreted as a failed stage.
func TestAppPhaseFailureRequiresEvidence(t *testing.T) {
	host := NewRecordingCapabilityHost(nil)
	host.RecordExecutionPhases(context.Background(), []executionappvm.PhaseTiming{{Name: "compilation", StartedAt: time.Now(), Duration: time.Millisecond}})
	// A measured stage may have completed before an unrelated interruption; no failed flag means no attribution.
	if host.receiptFailureReason() != "" {
		t.Fatal("completed stage was incorrectly marked failed")
	}
}
