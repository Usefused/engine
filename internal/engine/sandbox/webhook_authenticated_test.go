package sandbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/testcontract"
	"github.com/nats-io/nats.go"
)

// TestAuthenticatedChallengeAndEventIngress proves the shared HTTP path never publishes rejected or challenge traffic.
func TestAuthenticatedChallengeAndEventIngress(t *testing.T) {
	withEntitlement(t, models.RuntimeEntitlement{WebhookIngestionEnabled: true})
	t.Setenv("FUSED_ENV", "development")
	policy := testcontract.AuthenticatedSignature()
	const slug = "signed-v2"
	seedConfig(slug, &webhookConfig{AuthType: "signature_policy", SignaturePolicy: &policy, EventExtractionPath: "body.event.type"}, "test-signing-key")
	original := webhookPublishFunc
	published := 0
	// Capture only event publication; challenge and rejection audit records use a distinct path.
	webhookPublishFunc = func(*nats.Msg) error { published++; return nil }
	// Restore the process-wide publisher even when an assertion fails.
	t.Cleanup(func() { webhookPublishFunc = original })
	cases := []struct {
		name, body        string
		valid             bool
		status, published int
	}{
		{"unsigned challenge", `{"type":"url_verification","challenge":"proof"}`, false, 401, 0},
		{"signed challenge", `{"type":"url_verification","challenge":"proof"}`, true, 200, 0},
		{"unsigned event", `{"event":{"type":"app_mention"}}`, false, 401, 0},
		{"signed event", `{"event":{"type":"app_mention"}}`, true, 200, 1},
	}
	for _, tc := range cases {
		// Sequential cases assert that only the final authenticated event enters the durable publish boundary.
		t.Run(tc.name, func(t *testing.T) {
			stamp := strconv.FormatInt(time.Now().Unix(), 10)
			signature := "invalid"
			// Positive controls authenticate the unmodified body with the documented provider input construction.
			if tc.valid {
				mac := hmac.New(sha256.New, []byte("test-signing-key"))
				_, _ = mac.Write([]byte("v0:" + stamp + ":" + tc.body))
				signature = hex.EncodeToString(mac.Sum(nil))
			}
			req := makeWebhookRequest(http.MethodPost, slug, tc.body, map[string]string{"X-Slack-Signature": "v0=" + signature, "X-Slack-Request-Timestamp": stamp})
			response := httptest.NewRecorder()
			webhookIngressHandler(response, req)
			// HTTP success alone is insufficient: a challenge must not also publish an event.
			if response.Code != tc.status || published != tc.published {
				t.Fatalf("status=%d published=%d want %d/%d", response.Code, published, tc.status, tc.published)
			}
		})
	}
}

// TestRemoteWebhookRegistrationRejectsPublicIngress proves opaque registration URLs cannot bypass the authenticated pull adapter.
func TestRemoteWebhookRegistrationRejectsPublicIngress(t *testing.T) {
	withEntitlement(t, models.RuntimeEntitlement{WebhookIngestionEnabled: true})
	seedConfig("remote-only", &webhookConfig{AuthType: "fused_remote"}, "")
	req := makeWebhookRequest(http.MethodPost, "remote-only", `{"event":{"type":"app_mention"}}`, nil)
	response := httptest.NewRecorder()
	webhookIngressHandler(response, req)
	// A remote registration remains closed even when its random ingress slug is known.
	if response.Code != http.StatusForbidden {
		t.Fatalf("remote ingress status=%d", response.Code)
	}
}
