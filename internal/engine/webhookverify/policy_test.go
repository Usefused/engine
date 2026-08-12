package webhookverify_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/webhookverify"
	"github.com/Usefused/engine/internal/shared/signaturepolicy"
	"github.com/Usefused/engine/internal/testcontract"
)

// loadSignatureFixture resolves semantic recipe names through the single local
// test-contract owner rather than filesystem or provider-name dispatch.
func loadSignatureFixture(t *testing.T, name string) signaturepolicy.Config {
	t.Helper()
	return testcontract.SignaturePolicy(name)
}

func fixtureResolver(secret string) func(context.Context, string) (string, error) {
	return func(context.Context, string) (string, error) { return secret, nil }
}

// TestRawBodyCallbackSignatureUsesTrustedURL proves untrusted request metadata
// cannot replace the callback URL committed by the runtime contract.
func TestRawBodyCallbackSignatureUsesTrustedURL(t *testing.T) {
	policy := loadSignatureFixture(t, "raw_body_callback")
	body := []byte(`{"action":"update"}`)
	callbackURL := "https://hooks.example.test/webhook/immutable"
	mac := hmac.New(sha1.New, []byte("secret"))
	_, _ = mac.Write(append(append([]byte{}, body...), callbackURL...))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, "https://attacker.invalid/spoofed", nil)
	req.Host = "attacker.invalid"
	req.Header.Set("X-Forwarded-Host", "also-attacker.invalid")
	req.Header.Set("X-Callback-Signature", signature)

	result := webhookverify.VerifyPolicy(context.Background(), &policy, webhookverify.PolicyInput{
		Request: req, RawBody: body, CallbackURL: callbackURL, Resolve: fixtureResolver("secret"),
	})
	if !result.OK {
		t.Fatalf("trusted callback recipe rejected: %#v", result)
	}
}

// TestURLFormSignatureSortsRepeatedPairs protects deterministic signing when a
// form repeats the same field name.
func TestURLFormSignatureSortsRepeatedPairs(t *testing.T) {
	policy := loadSignatureFixture(t, "url_form")
	callbackURL := "https://hooks.example.test/webhook/form"
	body := []byte("Digits=2&CallSid=B&CallSid=A")
	mac := hmac.New(sha1.New, []byte("token"))
	_, _ = mac.Write([]byte(callbackURL + "CallSidACallSidBDigits2"))
	req := httptest.NewRequest(http.MethodPost, "/ignored", nil)
	req.Header.Set("X-Webhook-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	result := webhookverify.VerifyPolicy(context.Background(), &policy, webhookverify.PolicyInput{
		Request: req, RawBody: body, CallbackURL: callbackURL, Resolve: fixtureResolver("token"),
	})
	if !result.OK {
		t.Fatalf("sorted form recipe rejected: %#v", result)
	}
}

// TestConditionalChallengePrecedesJWT preserves first-match rule ordering.
func TestConditionalChallengePrecedesJWT(t *testing.T) {
	policy := loadSignatureFixture(t, "conditional_challenge_jwt")
	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	result := webhookverify.VerifyPolicy(context.Background(), &policy, webhookverify.PolicyInput{
		Request: req, RawBody: []byte(`{"challenge":"abc"}`),
	})
	if result.Code != webhookverify.CodeChallengeResponded || result.StatusCode != 200 || string(result.ChallengeBody) != `{"challenge":"abc"}` {
		t.Fatalf("challenge result = %#v", result)
	}
}

// TestConditionalEventVerifiesBoundJWT checks issuer and audience remain bound.
func TestConditionalEventVerifiesBoundJWT(t *testing.T) {
	policy := loadSignatureFixture(t, "conditional_challenge_jwt")
	now := time.Unix(2_000_000_000, 0)
	token := signedJWT(t, "secret", map[string]any{"alg": "HS256", "typ": "JWT"}, map[string]any{
		"aud": "webhook", "exp": now.Add(time.Minute).Unix(),
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("Authorization", "bEaReR "+token)
	result := webhookverify.VerifyPolicy(context.Background(), &policy, webhookverify.PolicyInput{
		Request: req, RawBody: []byte(`{"event":{"id":1}}`), Now: now, Resolve: fixtureResolver("secret"),
	})
	if !result.OK {
		t.Fatalf("JWT event rejected: %#v", result)
	}
}

func signedJWT(t *testing.T, secret string, header, claims map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(data)
	}
	unsigned := encode(header) + "." + encode(claims)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
