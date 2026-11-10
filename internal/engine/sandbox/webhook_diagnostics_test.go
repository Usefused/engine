package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type webhookDiagnosticCaptureStore struct {
	store.Store
	scope   store.WebhookDiagnosticScope
	payload json.RawMessage
	fail    bool
}

// SaveWebhookDiagnostics substitutes only encrypted-store persistence so the real ingress and canonical publisher remain exercised.
func (s *webhookDiagnosticCaptureStore) SaveWebhookDiagnostics(_ context.Context, scope store.WebhookDiagnosticScope, payload json.RawMessage, _ []byte) error {
	s.scope, s.payload = scope, append(json.RawMessage(nil), payload...)
	// A simulated storage outage must never change verification or provider acknowledgement.
	if s.fail {
		return errors.New("private synthetic storage error")
	}
	return nil
}

// GetWebhookDiagnostics is deliberately unavailable in the ingress-only fixture.
func (s *webhookDiagnosticCaptureStore) GetWebhookDiagnostics(context.Context, store.WebhookDiagnosticScope, []byte) (json.RawMessage, error) {
	return nil, store.ErrExecutionResultNotFound
}

// DeleteExpiredWebhookDiagnostics is unused by this ingress-only fixture.
func (s *webhookDiagnosticCaptureStore) DeleteExpiredWebhookDiagnostics(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

// TestWebhookDiagnosticCaptureBounds proves binary fidelity, truncation, original-body capture and header/query exclusion.
func TestWebhookDiagnosticCaptureBounds(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`{"private":"body"}`), {0xff, 0x00}, bytes.Repeat([]byte("a"), maxWebhookDiagnosticBodyBytes+1)} {
		r := httptest.NewRequest(http.MethodPost, "/webhook/test?token=QUERY_SECRET", bytes.NewReader(raw))
		r.Header.Set("Authorization", "HEADER_SECRET")
		r.Header.Set("Cookie", "COOKIE_SECRET")
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
		ctx := withWebhookDiagnosticCapture(context.Background(), r, raw)
		capture := ctx.Value(webhookDiagnosticCaptureKey{}).(webhookDiagnosticCapture)
		require.Equal(t, len(raw), capture.OriginalBytes)
		require.Equal(t, min(len(raw), maxWebhookDiagnosticBodyBytes), capture.CapturedBytes)
		require.Equal(t, len(raw) > maxWebhookDiagnosticBodyBytes, capture.Truncated)
		actual := []byte(capture.Body)
		// Invalid UTF-8 must round-trip as exact bytes rather than replacement characters.
		if capture.Encoding == "base64" {
			var err error
			actual, err = base64.StdEncoding.DecodeString(capture.Body)
			require.NoError(t, err)
		}
		require.Equal(t, raw[:capture.CapturedBytes], actual)
		encoded, _ := json.Marshal(capture)
		for _, secret := range []string{"HEADER_SECRET", "COOKIE_SECRET", "QUERY_SECRET"} {
			require.NotContains(t, string(encoded), secret)
		}
	}
}

// TestWebhookDiagnosticsIngressPrivacy proves accepted and rejected requests retain original evidence outside canonical events.
func TestWebhookDiagnosticsIngressPrivacy(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	originalProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(originalProvider); _ = provider.Shutdown(context.Background()) })
	withEntitlement(t, models.RuntimeEntitlement{WebhookIngestionEnabled: true})
	originalRepository, originalKey := webhookDiagnosticsRepository, webhookDiagnosticsKey
	originalPublish := webhookPublishFunc
	t.Cleanup(func() {
		webhookDiagnosticsRepository, webhookDiagnosticsKey = originalRepository, originalKey
		webhookPublishFunc = originalPublish
		executionevent.SetPublisher(nil)
	})
	for _, tc := range []struct {
		name      string
		fail      bool
		signature string
		status    int
	}{{"accepted", false, "", 200}, {"rejected", false, "invalid", 401}, {"capture outage", true, "", 200}} {
		// Each attempt has independent body evidence and a fresh canonical receipt.
		t.Run(tc.name, func(t *testing.T) {
			const body = `{"private":"PRIVATE_SENTINEL"}`
			const slug = "diagnostic-ingress"
			seedConfig(slug, &webhookConfig{AuthType: "hmac_signature", SignatureHeader: "X-Signature"}, "synthetic-signing-key")
			registration, err := testWebhookConfigStore.GetWorkspaceWebhookBySlug(context.Background(), slug)
			require.NoError(t, err)
			registration.ID = uuid.New()
			repository := &webhookDiagnosticCaptureStore{fail: tc.fail}
			SetWebhookDiagnosticsStore(repository, bytes.Repeat([]byte{7}, 32))
			canonical := &captureJetStreamPublisher{}
			executionevent.SetPublisher(executionevent.NewPublisher(canonical))
			// The delivery bus may carry the message, but canonical Activity must remain metadata-only.
			webhookPublishFunc = func(_ *nats.Msg) error { return nil }
			signature := tc.signature
			// Only deliberately rejected attempts use the invalid signature fixture.
			if signature == "" {
				signature = hmacBodySig("synthetic-signing-key", body)
			}
			r := makeWebhookRequest(http.MethodPost, slug, body, map[string]string{"X-Signature": signature, "Authorization": "HEADER_SECRET"})
			w := httptest.NewRecorder()
			webhookIngressHandler(w, r)
			require.Equal(t, tc.status, w.Code)
			require.NotNil(t, canonical.message)
			require.Contains(t, string(repository.payload), "PRIVATE_SENTINEL")
			require.NotContains(t, string(repository.payload), "HEADER_SECRET")
			var envelope models.EngineExecutionEventEnvelope
			require.NoError(t, json.Unmarshal(canonical.message.Data, &envelope))
			require.Equal(t, envelope.Event.ID, repository.scope.EventID)
			require.Equal(t, envelope.Event.AccountID, repository.scope.AccountID)
			require.Equal(t, registration.ID, repository.scope.RegistrationID)
			require.NotContains(t, string(canonical.message.Data), "PRIVATE_SENTINEL")
			require.NotContains(t, string(canonical.message.Data), "HEADER_SECRET")
			require.NotContains(t, w.Body.String(), "private synthetic storage error")
		})
	}
	// Private body and request credentials must not appear in any ingress, verification or publisher span.
	for _, span := range exporter.GetSpans() {
		encoded, _ := json.Marshal(span)
		for _, secret := range []string{"PRIVATE_SENTINEL", "HEADER_SECRET", "synthetic-signing-key", "private synthetic storage error"} {
			require.NotContains(t, string(encoded), secret)
		}
	}
}
