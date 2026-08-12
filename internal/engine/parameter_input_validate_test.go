package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestValidateDeclaredExecutionParametersUsesReviewedContract(t *testing.T) {
	selected := &SelectedRequestRepresentation{
		PayloadParameter: "payload",
		Encoding:         map[string]models.RequestEncoding{"file": {}},
		Schema: &models.Schema{Type: "object", Properties: map[string]models.Schema{
			"name": {Type: "string"},
		}},
	}
	declared := models.Parameters{{Name: "id", In: "path"}}
	for _, name := range []string{"id", "payload", "file", "name"} {
		if err := ValidateDeclaredExecutionParameters(declared, selected, map[string]any{name: "value"}); err != nil {
			t.Fatalf("reviewed parameter %q rejected: %v", name, err)
		}
	}
	if err := ValidateDeclaredExecutionParameters(declared, selected, map[string]any{"unknown": true}); err == nil || !strings.Contains(err.Error(), `undeclared execution parameter "unknown"`) {
		t.Fatalf("unknown parameter error = %v", err)
	}
}

func TestValidateDeclaredExecutionParametersAllowsReviewedFreeFormObject(t *testing.T) {
	selected := &SelectedRequestRepresentation{Schema: &models.Schema{Type: "object", AdditionalProperties: &models.Schema{}}}
	if err := ValidateDeclaredExecutionParameters(nil, selected, map[string]any{"provider_field": "value"}); err != nil {
		t.Fatalf("reviewed free-form field rejected: %v", err)
	}
}

func TestUndeclaredExecutionParameterFailsBeforeProviderAndEmitsBoundedOTEL(t *testing.T) {
	var providerCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		providerCalls.Add(1)
	}))
	defer provider.Close()

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(tracerProvider)
	defer otel.SetTracerProvider(previousProvider)

	operation := explicitAnonymousEndpoint(&models.IntegrationObject{Method: http.MethodGet, Path: "/items"})
	_, err := NewDispatcher().ExecuteStream(context.Background(), &models.Service{BaseURL: provider.URL}, operation, map[string]any{"secret_name": "secret_value"}, nil, nil, &mockStream{})
	if err == nil || !strings.Contains(err.Error(), `undeclared execution parameter "secret_name"`) {
		t.Fatalf("execution error = %v", err)
	}
	if providerCalls.Load() != 0 {
		t.Fatalf("provider calls = %d, want 0", providerCalls.Load())
	}
	attributes := safeStringSpanAttributes(t, recordedSpan(t, recorder.Ended(), "engine.dispatch.vendor_call"))
	if attributes["http.parameter_serialization.outcome"] != "rejected" {
		t.Fatalf("parameter outcome attributes = %#v", attributes)
	}
	serialized := strings.Join([]string{attributes["http.parameter_serialization.outcome"], attributes["provider.protocol"]}, " ")
	if strings.Contains(serialized, "secret_name") || strings.Contains(serialized, "secret_value") {
		t.Fatalf("parameter telemetry leaked caller input: %s", serialized)
	}
}
