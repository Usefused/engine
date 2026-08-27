package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type oauthCutoverTestStore struct {
	store.Store
	result    store.LegacyConnectConfigCutoverResult
	batchSize int
}

// TestConfiguredConnectRedirectURIAllowsOAuthToRemainOptIn protects existing Engines that do not use browser consent.
func TestConfiguredConnectRedirectURIAllowsOAuthToRemainOptIn(t *testing.T) {
	callback, err := configuredConnectRedirectURI("   ")
	// Missing configuration disables consent admission instead of blocking Engine startup.
	if err != nil || callback != "" {
		t.Fatalf("unset public URL resolved to %q, %v", callback, err)
	}
	callback, err = configuredConnectRedirectURI("https://engine.example.com/base/")
	// Configured deployments retain one canonical, operator-controlled callback.
	if err != nil || callback != "https://engine.example.com/base/workspace/connect/callback" {
		t.Fatalf("configured callback = %q, %v", callback, err)
	}
	_, err = configuredConnectRedirectURI("http://engine.example.com")
	// A non-loopback HTTP origin remains a startup error because it was explicitly configured.
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("unsafe configured public URL error = %v", err)
	}
}

// MigrateLegacyConnectConfigs captures the startup bound and returns aggregate-only fixture telemetry.
func (s *oauthCutoverTestStore) MigrateLegacyConnectConfigs(_ context.Context, _ []byte, batchSize int) (store.LegacyConnectConfigCutoverResult, error) {
	s.batchSize = batchSize
	return s.result, nil
}

// TestRunLegacyConnectConfigCutoverEmitsBoundedTelemetry verifies live cutover auditing contains no identifiers.
func TestRunLegacyConnectConfigCutoverEmitsBoundedTelemetry(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	repository := &oauthCutoverTestStore{result: store.LegacyConnectConfigCutoverResult{MigratedRows: 3, BatchCount: 2}}
	if err := runLegacyConnectConfigCutover(context.Background(), repository, []byte("test-master-key")); err != nil {
		t.Fatalf("runLegacyConnectConfigCutover: %v", err)
	}
	assertOAuthCutoverSpan(t, recorder, repository.batchSize)
}

// assertOAuthCutoverSpan checks only fixed outcome and bounded aggregate attributes.
func assertOAuthCutoverSpan(t *testing.T, recorder *tracetest.SpanRecorder, batchSize int) {
	t.Helper()
	spans := recorder.Ended()
	// One startup gate owns the full cutover, so duplicate spans would make migration auditing ambiguous.
	if len(spans) != 1 {
		t.Fatalf("cutover spans = %d", len(spans))
	}
	attributes := map[string]any{}
	for _, item := range spans[0].Attributes() {
		attributes[string(item.Key)] = item.Value.AsInterface()
	}
	// Fixed bounds and aggregates are sufficient to diagnose startup without service, bucket, or secret identity.
	if batchSize != 100 || attributes["outcome"] != "succeeded" || attributes["rows_migrated"] != int64(3) || attributes["batch_count"] != int64(2) {
		t.Fatalf("cutover telemetry batch=%d attributes=%#v", batchSize, attributes)
	}
}
