package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestEngineExecuteCoreEnforcesExecutionPolicyTimeout(t *testing.T) {
	releaseVendor := make(chan struct{})
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-releaseVendor:
		}
	}))
	defer vendor.Close()
	defer close(releaseVendor)

	cache, endpoint := makePassthroughCache(t, vendor.URL)
	timeoutMs := 10
	cache.obj.TimeoutMs = &timeoutMs
	appID := uuid.New()

	started := time.Now()
	err := engineExecuteCore(
		context.Background(), cache, engine.NewDispatcher(), &dummyTokenValidator{},
		appID.String(), "token", endpoint, map[string]any{}, nil, "", engine.NewBufferStream(),
	)
	var timeoutErr *executionTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("execution error = %T %v, want executionTimeoutError", err, err)
	}
	if timeoutErr.TimeoutMs != timeoutMs {
		t.Fatalf("timeout_ms = %d, want %d", timeoutErr.TimeoutMs, timeoutMs)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Engine policy was not enforced promptly; elapsed=%s", elapsed)
	}
	category, code := classifyExecutionFailure(err, 0)
	if category != "timeout" || code != "deadline_exceeded" {
		t.Fatalf("failure classification = %q/%q", category, code)
	}
}

func TestExecutionPolicyTimeoutCancelsAndReturnsTypedError(t *testing.T) {
	timeoutMs := 5
	ctx, cancel, applied := contextWithExecutionPolicyTimeout(context.Background(), &timeoutMs, trace.SpanFromContext(context.Background()))
	defer cancel()

	select {
	case <-ctx.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("execution policy did not cancel the request context")
	}

	err := normalizeExecutionTimeout(ctx, ctx.Err(), applied)
	var timeoutErr *executionTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %T %v, want executionTimeoutError", err, err)
	}
	if timeoutErr.TimeoutMs != timeoutMs || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %#v, deadline=%v", timeoutErr, errors.Is(err, context.DeadlineExceeded))
	}

	var payload executionTimeoutError
	if unmarshalErr := json.Unmarshal([]byte(encodeRuntimeError(err)), &payload); unmarshalErr != nil {
		t.Fatalf("decode runtime error: %v", unmarshalErr)
	}
	if payload.Code != "execution_timeout" || payload.TimeoutMs != timeoutMs {
		t.Fatalf("runtime error = %#v", payload)
	}
}

func TestExecutionPolicyTimeoutDoesNotMislabelEarlierCallerDeadline(t *testing.T) {
	callerCtx, callerCancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer callerCancel()
	policyTimeoutMs := 1000
	ctx, cancel, applied := contextWithExecutionPolicyTimeout(callerCtx, &policyTimeoutMs, trace.SpanFromContext(context.Background()))
	defer cancel()

	<-ctx.Done()
	err := normalizeExecutionTimeout(ctx, ctx.Err(), applied)
	var timeoutErr *executionTimeoutError
	if errors.As(err, &timeoutErr) {
		t.Fatalf("caller deadline was mislabeled as policy timeout: %#v", timeoutErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want caller deadline exceeded", err)
	}
}

func TestExecutionPolicyTimeoutAbsentDoesNotAddDeadline(t *testing.T) {
	ctx, cancel, applied := contextWithExecutionPolicyTimeout(context.Background(), nil, trace.SpanFromContext(context.Background()))
	defer cancel()

	if applied != 0 {
		t.Fatalf("applied timeout = %d, want 0", applied)
	}
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("nil execution policy timeout must not add a deadline")
	}
}

func TestExecutionPolicyTimeoutAddsSafeOTELAttribute(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	_, span := provider.Tracer("test").Start(context.Background(), "execute")
	timeoutMs := 45000
	_, cancel, _ := contextWithExecutionPolicyTimeout(context.Background(), &timeoutMs, span)
	cancel()
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("recorded spans = %d, want 1", len(spans))
	}
	for _, attr := range spans[0].Attributes {
		if string(attr.Key) == "execution.timeout_ms" && attr.Value.AsInt64() == int64(timeoutMs) {
			return
		}
	}
	t.Fatalf("execution.timeout_ms=%d missing from span attributes: %#v", timeoutMs, spans[0].Attributes)
}
