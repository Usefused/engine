package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/testcontract"
	"github.com/stretchr/testify/require"
)

// TestWebhookRejectionAllowlistEventExtraction verifies rejected webhooks capture catalog-allowlisted event names and fall back to UNKNOWN otherwise.
func TestWebhookRejectionAllowlistEventExtraction(t *testing.T) {
	withEntitlement(t, models.RuntimeEntitlement{WebhookIngestionEnabled: true})
	t.Cleanup(func() {
		executionevent.SetPublisher(nil)
		testWebhookConfigStore.mu.Lock()
		testWebhookConfigStore.catalogErr = nil
		testWebhookConfigStore.mu.Unlock()
	})

	// Test 1: Structured policy rejection (Stripe stale timestamp -> timestamp_invalid) with an allowlisted event name.
	t.Run("structured policy keeps allowlisted catalog event", func(t *testing.T) {
		policy := testcontract.StructuredSignature()
		policy.Rules[0].Verification.Signature.SecretRef = "${bucket.default.secret.test-label}"
		const slug = "rej-allow-structured"
		seedConfig(slug, &webhookConfig{SignaturePolicy: &policy, EventExtractionPath: "body.type"}, "stripe-secret")
		ww, err := testWebhookConfigStore.GetWorkspaceWebhookBySlug(context.Background(), slug)
		require.NoError(t, err)
		testWebhookConfigStore.setKnownEvents(ww.ServiceID, ww.ServiceVersionID, "invoice.paid", "checkout.session.completed")

		canonical := &captureJetStreamPublisher{}
		executionevent.SetPublisher(executionevent.NewPublisher(canonical))

		staleStamp := strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10)
		req := makeWebhookRequest(http.MethodPost, slug, `{"id":"evt_1","type":"invoice.paid"}`, map[string]string{
			"Stripe-Signature": "t=" + staleStamp + ",v1=" + strings.Repeat("a", 64),
		})
		w := httptest.NewRecorder()
		webhookIngressHandler(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.NotNil(t, canonical.message)

		var envelope models.EngineExecutionEventEnvelope
		require.NoError(t, json.Unmarshal(canonical.message.Data, &envelope))
		require.Equal(t, "invoice.paid", envelope.Event.EventName)
		require.Equal(t, "rejected", envelope.Event.DeliveryStatus)
		require.Equal(t, "rejected", envelope.Event.VerificationStatus)
		require.Equal(t, "timestamp_invalid", envelope.Event.FailureReason)
	})

	// Test 2: Unregistered/spoofed event name must never enter canonical receipts or leak private values.
	t.Run("unknown or spoofed event falls back to UNKNOWN", func(t *testing.T) {
		const slug = "rej-allow-spoofed"
		seedConfig(slug, &webhookConfig{AuthType: "hmac_signature", SignatureHeader: "X-Signature", EventExtractionPath: "body.type"}, "hmac-secret")
		ww, err := testWebhookConfigStore.GetWorkspaceWebhookBySlug(context.Background(), slug)
		require.NoError(t, err)
		testWebhookConfigStore.setKnownEvents(ww.ServiceID, ww.ServiceVersionID, "invoice.paid")

		canonical := &captureJetStreamPublisher{}
		executionevent.SetPublisher(executionevent.NewPublisher(canonical))

		req := makeWebhookRequest(http.MethodPost, slug, `{"type":"PRIVATE_SPOOFED_EVENT"}`, map[string]string{"X-Signature": "invalid"})
		w := httptest.NewRecorder()
		webhookIngressHandler(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.NotNil(t, canonical.message)

		var envelope models.EngineExecutionEventEnvelope
		require.NoError(t, json.Unmarshal(canonical.message.Data, &envelope))
		require.Equal(t, "UNKNOWN", envelope.Event.EventName)
		require.NotContains(t, string(canonical.message.Data), "PRIVATE_SPOOFED_EVENT")
	})

	// Test 3: Form-encoded payload with allowlisted event extracts from parsedBody while verifying against rawBody.
	t.Run("form-encoded rejection extracts allowlisted event from normalized body", func(t *testing.T) {
		const slug = "rej-allow-form"
		seedConfig(slug, &webhookConfig{AuthType: "hmac_signature", SignatureHeader: "X-Signature", EventExtractionPath: "body.MessageStatus"}, "hmac-secret")
		ww, err := testWebhookConfigStore.GetWorkspaceWebhookBySlug(context.Background(), slug)
		require.NoError(t, err)
		testWebhookConfigStore.setKnownEvents(ww.ServiceID, ww.ServiceVersionID, "delivered")

		canonical := &captureJetStreamPublisher{}
		executionevent.SetPublisher(executionevent.NewPublisher(canonical))

		req := makeWebhookRequest(http.MethodPost, slug, "MessageSid=SM123&MessageStatus=delivered", map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
			"X-Signature":  "invalid",
		})
		w := httptest.NewRecorder()
		webhookIngressHandler(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.NotNil(t, canonical.message)

		var envelope models.EngineExecutionEventEnvelope
		require.NoError(t, json.Unmarshal(canonical.message.Data, &envelope))
		require.Equal(t, "delivered", envelope.Event.EventName)
	})

	// Test 4: Oversized candidate names skip the database lookup and catalog errors fail closed to UNKNOWN.
	t.Run("oversized names skip lookup and catalog errors fail closed", func(t *testing.T) {
		const slug = "rej-allow-guards"
		seedConfig(slug, &webhookConfig{AuthType: "hmac_signature", SignatureHeader: "X-Signature", EventExtractionPath: "body.type"}, "hmac-secret")
		ww, err := testWebhookConfigStore.GetWorkspaceWebhookBySlug(context.Background(), slug)
		require.NoError(t, err)
		testWebhookConfigStore.setKnownEvents(ww.ServiceID, ww.ServiceVersionID, "invoice.paid")

		testWebhookConfigStore.mu.Lock()
		callsBefore := testWebhookConfigStore.catalogCalls
		testWebhookConfigStore.mu.Unlock()

		canonical := &captureJetStreamPublisher{}
		executionevent.SetPublisher(executionevent.NewPublisher(canonical))

		oversized := strings.Repeat("a", maxRejectedEventNameBytes+1)
		req := makeWebhookRequest(http.MethodPost, slug, `{"type":"`+oversized+`"}`, map[string]string{"X-Signature": "invalid"})
		w := httptest.NewRecorder()
		webhookIngressHandler(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		var envelope models.EngineExecutionEventEnvelope
		require.NoError(t, json.Unmarshal(canonical.message.Data, &envelope))
		require.Equal(t, "UNKNOWN", envelope.Event.EventName)

		testWebhookConfigStore.mu.Lock()
		// Oversized unverified names must be rejected before issuing any catalog query.
		require.Equal(t, callsBefore, testWebhookConfigStore.catalogCalls)
		testWebhookConfigStore.catalogErr = errors.New("synthetic db timeout")
		testWebhookConfigStore.mu.Unlock()

		canonical.message = nil
		req = makeWebhookRequest(http.MethodPost, slug, `{"type":"invoice.paid"}`, map[string]string{"X-Signature": "invalid"})
		w = httptest.NewRecorder()
		webhookIngressHandler(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.NoError(t, json.Unmarshal(canonical.message.Data, &envelope))
		require.Equal(t, "UNKNOWN", envelope.Event.EventName)
	})
}
