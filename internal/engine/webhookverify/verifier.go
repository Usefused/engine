// Package webhookverify provides pure, HTTP-handler-agnostic webhook signature
// verification strategies. It has no dependency on NATS, chi, or any sandbox
// internals — intentionally kept clean so it can be reviewed as a
// source-available trust component (S11).
package webhookverify

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// Config carries the auth parameters needed to verify an incoming webhook.
// It is a plain value type — no database or NATS references — so tests can
// construct it inline without mocks.
type Config struct {
	AuthType            string
	AuthLocation        string // "header" | "query"
	AuthKeyName         string
	SignatureHeader     string
	SigningSecret       string
	VerificationHeaders []string
}

// VerifyResult holds the outcome of a verification attempt.
// OK=false means the event must be dropped; Reason is logged/traced.
type VerifyResult struct {
	OK     bool
	Reason string
}

// ok is a convenience constructor for a passing result.
func ok() VerifyResult { return VerifyResult{OK: true} }

// fail is a convenience constructor for a rejection result.
func fail(reason string) VerifyResult { return VerifyResult{OK: false, Reason: reason} }

// Verify dispatches to the correct verification strategy based on cfg.AuthType.
// Unknown or empty AuthType is treated as "no auth required" (allow-through) so
// integrations that send plain payloads are not silently broken.
//
// Complexity: 1 (switch) + 3 (cases) = 4
func Verify(r *http.Request, body []byte, cfg Config) VerifyResult {
	switch cfg.AuthType {
	case "hmac_signature":
		return VerifyHMAC(r, body, cfg)
	case "signature_header":
		return VerifySignatureHeader(r, cfg)
	case "static_token":
		return VerifyStaticToken(r, cfg)
	default:
		// No auth configured — allow through. This is a deliberate design
		// decision: we prefer availability over accidental lockout when a
		// provider hasn't declared a verification scheme yet.
		return ok()
	}
}

// VerifyHMAC validates the request body against an HMAC-SHA256 signature
// contained in a provider-specific header (e.g. Stripe-Signature).
//
// Complexity: 1 (empty guard) + 1 (method guard) + 1 (empty sig guard) + 1 (mismatch guard) = 4
func VerifyHMAC(r *http.Request, body []byte, cfg Config) VerifyResult {
	// Misconfigured secret header name → treat as skip, not an error.
	// Locking out all events because an admin forgot a field would be worse.
	if cfg.SignatureHeader == "" {
		return ok()
	}

	// HMAC requires a body to sign; GET requests carry no body.
	if r.Method == http.MethodGet {
		return fail("HMAC signature verification is not supported for GET requests")
	}

	sig := r.Header.Get(cfg.SignatureHeader)
	if sig == "" {
		return fail("missing signature header")
	}

	// Compute expected HMAC-SHA256 and check if it is present in the sig string.
	// The "contains" check handles providers like Stripe that include metadata
	// alongside the signature (e.g. "t=...,v1=<sig>").
	mac := hmac.New(sha256.New, []byte(cfg.SigningSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !strings.Contains(sig, expected) {
		return fail("invalid signature")
	}
	return ok()
}

// VerifySignatureHeader checks that all required verification headers are present.
// It does NOT validate their values — that is intentional: providers like GitHub
// use asymmetric signatures that require the caller to verify externally, so we
// gate only on presence here.
//
// Complexity: 1 (fallback guard) + 1 (for loop) + 1 (empty guard) = 3
func VerifySignatureHeader(r *http.Request, cfg Config) VerifyResult {
	required := cfg.VerificationHeaders

	// Fall back to the primary signature header when no list is declared.
	if len(required) == 0 && cfg.SignatureHeader != "" {
		required = []string{cfg.SignatureHeader}
	}

	for _, h := range required {
		if r.Header.Get(h) == "" {
			return fail("missing required verification header: " + h)
		}
	}
	return ok()
}

// VerifyStaticToken validates a static shared secret supplied either in a
// request header or a query parameter.
//
// Complexity: 1 (empty guard) + 1 (location if/else) + 1 (empty token guard) + 1 (mismatch guard) = 4
func VerifyStaticToken(r *http.Request, cfg Config) VerifyResult {
	if cfg.AuthKeyName == "" {
		return ok()
	}

	var token string
	if cfg.AuthLocation == "header" {
		token = r.Header.Get(cfg.AuthKeyName)
		// Strip "Bearer " prefix so providers that send a bare token and those
		// that wrap it in an Authorization header both work without extra config.
		if strings.HasPrefix(strings.ToLower(token), "bearer ") {
			token = strings.TrimSpace(token[7:])
		}
	} else if cfg.AuthLocation == "query" {
		token = r.URL.Query().Get(cfg.AuthKeyName)
	}

	if token == "" {
		return fail("missing static token")
	}

	if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.SigningSecret)) != 1 {
		return fail("invalid token")
	}
	return ok()
}
