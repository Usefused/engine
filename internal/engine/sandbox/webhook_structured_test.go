package sandbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/testcontract"
	"github.com/nats-io/nats.go"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestWebhookIngressStructuredHeader proves original provider headers reach verified publication without an adapter.
func TestWebhookIngressStructuredHeader(t *testing.T) {
	withEntitlement(t, models.RuntimeEntitlement{WebhookIngestionEnabled: true})
	p := testcontract.StructuredSignature()
	// Bind the recipe to the test registration's immutable secret reference.
	p.Rules[0].Verification.Signature.SecretRef = "${bucket.default.secret.test-label}"
	const slug = "structured-native"
	const body = `{"type":"invoice.paid","id":"evt_test_native"}`
	seedConfig(slug, &webhookConfig{SignaturePolicy: &p, EventExtractionPath: "body.type"}, "secret")
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte(stamp + "." + body))
	signature := "t=" + stamp + ",v1=" + strings.Repeat("0", 64) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	published := 0
	previous := webhookPublishFunc
	// Publication is the post-verification boundary; count it without requiring a live broker.
	webhookPublishFunc = func(*nats.Msg) error { published++; return nil }
	// Restore the shared handler hook so this test does not influence other ingress tests.
	t.Cleanup(func() { webhookPublishFunc = previous })
	req := makeWebhookRequest(http.MethodPost, slug, body, map[string]string{"Stripe-Signature": signature})
	w := httptest.NewRecorder()
	webhookIngressHandler(w, req)
	// Native verification must produce one successful event, not merely avoid a signature error.
	if w.Code < 200 || w.Code >= 300 || published != 1 {
		t.Fatalf("status=%d published=%d body=%s", w.Code, published, w.Body.String())
	}
	req = makeWebhookRequest(http.MethodPost, slug, body+" ", map[string]string{"Stripe-Signature": signature})
	w = httptest.NewRecorder()
	webhookIngressHandler(w, req)
	// Tampering must stop before the event reaches the broker.
	if w.Code != http.StatusUnauthorized || published != 1 {
		t.Fatalf("tampered status=%d published=%d", w.Code, published)
	}
}
