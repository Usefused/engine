package api

import (
	"context"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/signaturepolicy"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestContractWebhookOTELAppliedSpanDeniesRegistrationMaterial keeps user-triggered audit spans useful without leaking registration secrets.
func TestContractWebhookOTELAppliedSpanDeniesRegistrationMaterial(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	registration := store.WorkspaceWebhook{
		ID: uuid.New(), ServiceID: uuid.New(), Label: "customer-label",
		SecretRef: "${bucket.default.secret.private}", CallbackURL: "https://private.example/webhook/token",
		SignaturePolicy: &signaturepolicy.Config{Version: 1},
	}
	emitWebhookAppliedSpan(context.Background(), registration)
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d", len(spans))
	}
	for _, attr := range spans[0].Attributes() {
		value := attr.Value.Emit()
		if strings.Contains(value, registration.Label) || strings.Contains(value, registration.SecretRef) || strings.Contains(value, registration.CallbackURL) {
			t.Fatalf("registration material leaked through %s=%q", attr.Key, value)
		}
	}
}
