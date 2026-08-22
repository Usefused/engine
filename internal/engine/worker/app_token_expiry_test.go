package worker

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type appTokenExpiryStoreStub struct {
	results []int
	err     error
	calls   int
}

func (stub *appTokenExpiryStoreStub) ExpireAppTokens(_ context.Context, _ int) (int, error) {
	stub.calls++
	if stub.err != nil {
		return 0, stub.err
	}
	result := stub.results[0]
	stub.results = stub.results[1:]
	return result, nil
}

func TestExpireAppTokensPassDrainsBoundedBatches(t *testing.T) {
	exporter := installAppTokenExpiryTracer(t)
	store := &appTokenExpiryStoreStub{results: []int{2, 2, 1}}

	expireAppTokensPass(context.Background(), store, 2)

	if store.calls != 3 {
		t.Fatalf("expiry calls = %d, want 3", store.calls)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spanIntAttribute(spans[0], "app.token.expired_count") != 5 {
		t.Fatalf("expiry span = %#v, want one span with count 5", spans)
	}
}

func TestExpireAppTokensPassStopsAfterStoreError(t *testing.T) {
	store := &appTokenExpiryStoreStub{err: errors.New("database unavailable")}

	expireAppTokensPass(context.Background(), store, 100)

	if store.calls != 1 {
		t.Fatalf("expiry calls = %d, want 1", store.calls)
	}
}

func installAppTokenExpiryTracer(t *testing.T) *tracetest.InMemoryExporter {
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

func spanIntAttribute(span tracetest.SpanStub, key string) int64 {
	for _, item := range span.Attributes {
		if string(item.Key) == key {
			return item.Value.AsInt64()
		}
	}
	return 0
}
