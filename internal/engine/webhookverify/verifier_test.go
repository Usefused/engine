package webhookverify_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/webhookverify"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func makeReq(method, body string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(method, "/webhook/test-slug", bytes.NewBufferString(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func hmacSig(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// ─── HMAC ─────────────────────────────────────────────────────────────────────

func TestVerifyHMAC_ValidSignature(t *testing.T) {
	body := `{"event":"charge.succeeded"}`
	sig := hmacSig("test-secret", body)
	req := makeReq(http.MethodPost, body, map[string]string{
		"Stripe-Signature": sig,
	})
	cfg := webhookverify.Config{
		AuthType:        "hmac_signature",
		SignatureHeader: "Stripe-Signature",
		SigningSecret:   "test-secret",
	}
	result := webhookverify.Verify(req, []byte(body), cfg)
	if !result.OK {
		t.Errorf("expected OK=true, got reason: %s", result.Reason)
	}
}

func TestVerifyHMAC_TamperedBody(t *testing.T) {
	body := `{"event":"charge.succeeded"}`
	sig := hmacSig("test-secret", body)
	tamperedBody := `{"event":"charge.refunded"}`
	req := makeReq(http.MethodPost, tamperedBody, map[string]string{
		"Stripe-Signature": sig,
	})
	cfg := webhookverify.Config{
		AuthType:        "hmac_signature",
		SignatureHeader: "Stripe-Signature",
		SigningSecret:   "test-secret",
	}
	result := webhookverify.Verify(req, []byte(tamperedBody), cfg)
	if result.OK {
		t.Error("expected OK=false for tampered body")
	}
}

func TestVerifyHMAC_MissingSignatureHeader(t *testing.T) {
	body := `{"event":"charge.succeeded"}`
	req := makeReq(http.MethodPost, body, nil)
	cfg := webhookverify.Config{
		AuthType:        "hmac_signature",
		SignatureHeader: "Stripe-Signature",
		SigningSecret:   "test-secret",
	}
	result := webhookverify.Verify(req, []byte(body), cfg)
	if result.OK {
		t.Error("expected OK=false for missing signature header")
	}
	if result.Reason == "" {
		t.Error("expected a non-empty rejection reason")
	}
}

func TestVerifyHMAC_GetMethodRejected(t *testing.T) {
	req := makeReq(http.MethodGet, "", nil)
	cfg := webhookverify.Config{
		AuthType:        "hmac_signature",
		SignatureHeader: "X-Sig",
		SigningSecret:   "secret",
	}
	result := webhookverify.Verify(req, nil, cfg)
	if result.OK {
		t.Error("expected OK=false: HMAC verification is not supported for GET requests")
	}
}

func TestVerifyHMAC_NoSignatureHeaderConfigured_Rejects(t *testing.T) {
	req := makeReq(http.MethodPost, `{}`, nil)
	cfg := webhookverify.Config{
		AuthType:      "hmac_signature",
		SigningSecret: "secret",
		// SignatureHeader intentionally empty
	}
	result := webhookverify.Verify(req, []byte(`{}`), cfg)
	if result.OK || result.Code != webhookverify.CodeConfigIncomplete {
		t.Fatalf("expected incomplete configuration rejection, got: %#v", result)
	}
}

func TestVerifyHMAC_NoSecretConfigured_Rejects(t *testing.T) {
	req := makeReq(http.MethodPost, `{}`, nil)
	result := webhookverify.Verify(req, []byte(`{}`), webhookverify.Config{
		AuthType: "hmac_signature", SignatureHeader: "X-Signature",
	})
	if result.OK || result.Code != webhookverify.CodeConfigIncomplete {
		t.Fatalf("expected incomplete configuration rejection, got: %#v", result)
	}
}

// ─── Signature Header ─────────────────────────────────────────────────────────

func TestVerifySignatureHeader_AllHeadersPresent(t *testing.T) {
	req := makeReq(http.MethodPost, `{}`, map[string]string{
		"X-Hub-Signature-256": "sha256=abc",
		"X-GitHub-Event":      "push",
	})
	cfg := webhookverify.Config{
		AuthType:            "signature_header",
		VerificationHeaders: []string{"X-Hub-Signature-256", "X-GitHub-Event"},
	}
	result := webhookverify.Verify(req, []byte(`{}`), cfg)
	if !result.OK {
		t.Errorf("expected OK=true, got: %s", result.Reason)
	}
}

func TestVerifySignatureHeader_MissingOneHeader(t *testing.T) {
	req := makeReq(http.MethodPost, `{}`, map[string]string{
		"X-Hub-Signature-256": "sha256=abc",
		// X-GitHub-Event intentionally absent
	})
	cfg := webhookverify.Config{
		AuthType:            "signature_header",
		VerificationHeaders: []string{"X-Hub-Signature-256", "X-GitHub-Event"},
	}
	result := webhookverify.Verify(req, []byte(`{}`), cfg)
	if result.OK {
		t.Error("expected OK=false when a required verification header is missing")
	}
}

func TestVerifySignatureHeader_FallsBackToSignatureHeader(t *testing.T) {
	// When VerificationHeaders is empty but SignatureHeader is set, use that.
	req := makeReq(http.MethodPost, `{}`, map[string]string{
		"X-Sig": "present",
	})
	cfg := webhookverify.Config{
		AuthType:        "signature_header",
		SignatureHeader: "X-Sig",
		// VerificationHeaders intentionally empty
	}
	result := webhookverify.Verify(req, []byte(`{}`), cfg)
	if !result.OK {
		t.Errorf("expected OK=true when fallback SignatureHeader is present, got: %s", result.Reason)
	}
}

func TestVerifySignatureHeader_NoHeadersConfigured_Rejects(t *testing.T) {
	req := makeReq(http.MethodPost, `{}`, nil)
	result := webhookverify.Verify(req, []byte(`{}`), webhookverify.Config{AuthType: "signature_header"})
	if result.OK || result.Code != webhookverify.CodeConfigIncomplete {
		t.Fatalf("expected incomplete configuration rejection, got: %#v", result)
	}
}

// ─── Static Token ─────────────────────────────────────────────────────────────

func TestVerifyStaticToken_HeaderValid(t *testing.T) {
	req := makeReq(http.MethodPost, `{}`, map[string]string{
		"X-Webhook-Token": "my-secret",
	})
	cfg := webhookverify.Config{
		AuthType:      "static_token",
		AuthLocation:  "header",
		AuthKeyName:   "X-Webhook-Token",
		SigningSecret: "my-secret",
	}
	result := webhookverify.Verify(req, []byte(`{}`), cfg)
	if !result.OK {
		t.Errorf("expected OK=true for valid static token, got: %s", result.Reason)
	}
}

func TestVerifyStaticToken_BearerPrefixStripped(t *testing.T) {
	req := makeReq(http.MethodPost, `{}`, map[string]string{
		"Authorization": "Bearer my-secret",
	})
	cfg := webhookverify.Config{
		AuthType:      "static_token",
		AuthLocation:  "header",
		AuthKeyName:   "Authorization",
		SigningSecret: "my-secret",
	}
	result := webhookverify.Verify(req, []byte(`{}`), cfg)
	if !result.OK {
		t.Errorf("expected OK=true with Bearer prefix stripped, got: %s", result.Reason)
	}
}

func TestVerifyStaticToken_HeaderMissing(t *testing.T) {
	req := makeReq(http.MethodPost, `{}`, nil)
	cfg := webhookverify.Config{
		AuthType:      "static_token",
		AuthLocation:  "header",
		AuthKeyName:   "X-Webhook-Token",
		SigningSecret: "my-secret",
	}
	result := webhookverify.Verify(req, []byte(`{}`), cfg)
	if result.OK {
		t.Error("expected OK=false when token header is missing")
	}
}

func TestVerifyStaticToken_WrongValue(t *testing.T) {
	req := makeReq(http.MethodPost, `{}`, map[string]string{
		"X-Webhook-Token": "wrong-value",
	})
	cfg := webhookverify.Config{
		AuthType:      "static_token",
		AuthLocation:  "header",
		AuthKeyName:   "X-Webhook-Token",
		SigningSecret: "my-secret",
	}
	result := webhookverify.Verify(req, []byte(`{}`), cfg)
	if result.OK {
		t.Error("expected OK=false for mismatched token")
	}
}

func TestVerifyStaticToken_QueryValid(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/webhook/slug?api_key=my-secret", nil)
	cfg := webhookverify.Config{
		AuthType:      "static_token",
		AuthLocation:  "query",
		AuthKeyName:   "api_key",
		SigningSecret: "my-secret",
	}
	result := webhookverify.Verify(req, []byte(`{}`), cfg)
	if !result.OK {
		t.Errorf("expected OK=true for valid query token, got: %s", result.Reason)
	}
}

func TestVerifyStaticToken_IncompleteConfigurationRejects(t *testing.T) {
	req := makeReq(http.MethodPost, `{}`, nil)
	result := webhookverify.Verify(req, []byte(`{}`), webhookverify.Config{AuthType: "static_token"})
	if result.OK || result.Code != webhookverify.CodeConfigIncomplete {
		t.Fatalf("expected incomplete configuration rejection, got: %#v", result)
	}
}

// ─── Unknown/unset auth type ──────────────────────────────────────────────────

func TestVerify_UnsetAuthType_Passthrough(t *testing.T) {
	req := makeReq(http.MethodPost, `{}`, nil)
	cfg := webhookverify.Config{
		AuthType: "", // unset → allow through
	}
	result := webhookverify.Verify(req, []byte(`{}`), cfg)
	if !result.OK {
		t.Errorf("expected passthrough for unknown/unset auth type, got: %s", result.Reason)
	}
}

func TestVerify_ExplicitNoneAuthType_Passthrough(t *testing.T) {
	result := webhookverify.Verify(makeReq(http.MethodPost, `{}`, nil), []byte(`{}`), webhookverify.Config{AuthType: "none"})
	if !result.OK {
		t.Fatalf("expected passthrough for explicit none auth, got: %#v", result)
	}
}

func TestVerify_UnknownDeclaredAuthType_Rejects(t *testing.T) {
	result := webhookverify.Verify(makeReq(http.MethodPost, `{}`, nil), []byte(`{}`), webhookverify.Config{AuthType: "hmca_signature"})
	if result.OK || result.Code != webhookverify.CodeAuthUnsupported {
		t.Fatalf("expected unsupported auth rejection, got: %#v", result)
	}
}
