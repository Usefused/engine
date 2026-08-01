package sandbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/Usefused/engine/internal/engine/store"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func hmacBodySig(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// stubWebhookConfigStore is a package-level, in-memory stand-in for the real
// Postgres-backed store, letting tests seed a slug's resolved config directly
// instead of standing up a live DB or NATS connection.
type stubWebhookConfigStore struct {
	mu      sync.RWMutex
	byLabel map[string]*store.WorkspaceWebhook
}

func (s *stubWebhookConfigStore) GetWorkspaceWebhookBySlug(_ context.Context, slug string) (*store.WorkspaceWebhook, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ww, ok := s.byLabel[slug]
	if !ok {
		return nil, store.ErrWorkspaceWebhookNotFound
	}
	return ww, nil
}

func (s *stubWebhookConfigStore) set(slug string, ww *store.WorkspaceWebhook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byLabel == nil {
		s.byLabel = make(map[string]*store.WorkspaceWebhook)
	}
	s.byLabel[slug] = ww
}

var testWebhookConfigStore = &stubWebhookConfigStore{}

type webhookMockSecretResolver struct {
	secrets map[string]string // key: "<accountID>:<bucketID>:<secretRef>"
}

func (m *webhookMockSecretResolver) ResolveExecutionCredentials(_ context.Context, request CredentialRequest) (map[string]any, []store.BucketValue, error) {
	return request.Passthrough, nil, nil
}

func (m *webhookMockSecretResolver) GetWebhookSecret(ctx context.Context, accountID, bucketID uuid.UUID, secretRef string) (string, error) {
	key := accountID.String() + ":" + bucketID.String() + ":" + secretRef
	return m.secrets[key], nil
}

var testSecretResolver = &webhookMockSecretResolver{secrets: make(map[string]string)}

func init() {
	globalWebhookConfigStore = testWebhookConfigStore
	globalSecretResolver = testSecretResolver
}

func seedConfig(slug string, cfg *webhookConfig, signingSecret string) {
	accID := uuid.New()
	svcID := uuid.New()
	const secretRef = "${bucket.default.secret.test-label}"
	secretBucketID := uuid.New()
	ww := &store.WorkspaceWebhook{
		AccountID:           accID,
		ServiceID:           svcID,
		Label:               "test-label",
		EventExtractionPath: cfg.EventExtractionPath,
		AuthType:            cfg.AuthType,
		AuthLocation:        cfg.AuthLocation,
		AuthKeyName:         cfg.AuthKeyName,
		SignatureHeader:     cfg.SignatureHeader,
		VerificationHeaders: cfg.VerificationHeaders,
	}
	if signingSecret != "" {
		ww.SecretRef = secretRef
		ww.SecretBucketID = &secretBucketID
		key := accID.String() + ":" + secretBucketID.String() + ":" + secretRef
		testSecretResolver.secrets[key] = signingSecret
	}
	testWebhookConfigStore.set(slug, ww)
}

// makeWebhookRequest constructs a chi-routed request matching the Engine's
// /webhook/{urlSlug} pattern so chi.URLParam resolves correctly in tests.
func makeWebhookRequest(method, slug, body string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(method, "/webhook/"+slug, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Wire chi route context so URLParam("urlSlug") resolves.
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("urlSlug", slug)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// ─── Auth gate: bad HMAC → event must NOT be published ────────────────────────

// TestWebhookHandler_BadHMAC_EventDropped proves that a request with a tampered
// body (HMAC mismatch) is rejected before publishWebhookEvent is reached.
func TestWebhookHandler_BadHMAC_EventDropped(t *testing.T) {
	const slug = "slug-bad-hmac"
	seedConfig(slug, &webhookConfig{
		AuthType:        "hmac_signature",
		SignatureHeader: "X-Signature",
		AccountID:       "acc-1",
		ServiceID:       "svc-1",
	}, "real-secret")

	// Sign a DIFFERENT body to create a mismatch.
	sig := hmacBodySig("real-secret", `{"original":"body"}`)
	req := makeWebhookRequest(http.MethodPost, slug, `{"tampered":"body"}`, map[string]string{
		"X-Signature": sig,
	})
	w := httptest.NewRecorder()

	webhookIngressHandler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", w.Code)
	}
}

// ─── Auth gate: valid HMAC → event IS published ───────────────────────────────

// TestWebhookHandler_ValidHMAC_EventPublished proves that a correctly signed
// request flows through auth and reaches publishWebhookEvent. A no-op publish
// hook is injected so the test does not need a real NATS server.
func TestWebhookHandler_ValidHMAC_EventPublished(t *testing.T) {
	const slug = "slug-valid-hmac"
	const body = `{"event":"charge.succeeded"}`
	sig := hmacBodySig("real-secret", body)

	seedConfig(slug, &webhookConfig{
		AuthType:        "hmac_signature",
		SignatureHeader: "X-Signature",
		AccountID:       "acc-2",
		ServiceID:       "svc-2",
	}, "real-secret")

	// Replace the real NATS publish with a counter; restore after the test.
	var published int
	orig := webhookPublishFunc
	webhookPublishFunc = func(_ *nats.Msg) error { published++; return nil }
	defer func() { webhookPublishFunc = orig }()

	req := makeWebhookRequest(http.MethodPost, slug, body, map[string]string{
		"X-Signature": sig,
	})
	w := httptest.NewRecorder()

	webhookIngressHandler(w, req)

	// Auth passed — rejection codes must NOT appear.
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Errorf("expected auth to pass, got %d: %s", w.Code, w.Body.String())
	}
	if published != 1 {
		t.Errorf("expected publishWebhookEvent to be called once, got %d", published)
	}
}

// ─── Unknown slug gate ──────────────────────────────────────────────────────

// TestWebhookHandler_UnknownSlug_404 proves that a slug with no matching
// fused_workspace_webhooks row (never registered, or already removed) 404s
// before any auth logic runs -- there's no config to validate against.
func TestWebhookHandler_UnknownSlug_404(t *testing.T) {
	req := makeWebhookRequest(http.MethodPost, "slug-never-registered", `{}`, nil)
	w := httptest.NewRecorder()

	webhookIngressHandler(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for an unregistered slug, got %d", w.Code)
	}
}

// ─── Static token gate ────────────────────────────────────────────────────────

// TestWebhookHandler_StaticToken_MissingToken_Rejects proves that a request
// without the required token header is rejected before publish.
func TestWebhookHandler_StaticToken_MissingToken_Rejects(t *testing.T) {
	const slug = "slug-static-token"
	seedConfig(slug, &webhookConfig{
		AuthType:     "static_token",
		AuthLocation: "header",
		AuthKeyName:  "X-Webhook-Token",
		AccountID:    "acc-4",
		ServiceID:    "svc-4",
	}, "secret")

	req := makeWebhookRequest(http.MethodPost, slug, `{}`, nil) // no token header
	w := httptest.NewRecorder()

	webhookIngressHandler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// TestWebhookHandler_StaticToken_ValidToken_PassesAuth proves that a correctly
// supplied token passes the auth gate and reaches the publish step.
func TestWebhookHandler_StaticToken_ValidToken_PassesAuth(t *testing.T) {
	const slug = "slug-static-valid"
	seedConfig(slug, &webhookConfig{
		AuthType:     "static_token",
		AuthLocation: "header",
		AuthKeyName:  "X-Webhook-Token",
		AccountID:    "acc-5",
		ServiceID:    "svc-5",
	}, "good-secret")

	// Replace the real NATS publish with a no-op; restore after the test.
	var published int
	orig := webhookPublishFunc
	webhookPublishFunc = func(_ *nats.Msg) error { published++; return nil }
	defer func() { webhookPublishFunc = orig }()

	req := makeWebhookRequest(http.MethodPost, slug, `{}`, map[string]string{
		"X-Webhook-Token": "good-secret",
	})
	w := httptest.NewRecorder()

	webhookIngressHandler(w, req)

	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Errorf("expected auth to pass, got %d: %s", w.Code, w.Body.String())
	}
	if published != 1 {
		t.Errorf("expected publishWebhookEvent to be called once, got %d", published)
	}
}

func TestShouldObserveWebhookSchemaUsesDeploymentMode(t *testing.T) {
	t.Setenv("FUSED_ENV", "development")
	if shouldObserveWebhookSchema() {
		t.Fatal("development engines should not publish schema observations")
	}

	t.Setenv("FUSED_ENV", "production")
	if !shouldObserveWebhookSchema() {
		t.Fatal("production engines should publish schema observations")
	}
}
