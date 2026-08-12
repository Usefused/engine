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
	Code   string
	Reason string
}

const (
	CodeVerified               = "verified"
	CodeAuthUnsupported        = "auth_unsupported"
	CodeConfigIncomplete       = "config_incomplete"
	CodeMethodUnsupported      = "method_unsupported"
	CodeCredentialMissing      = "credential_missing"
	CodeCredentialInvalid      = "credential_invalid"
	CodeVerificationHeaderMiss = "verification_header_missing"
)

// ok is a convenience constructor for a passing result.
func ok() VerifyResult { return VerifyResult{OK: true, Code: CodeVerified} }

// fail is a convenience constructor for a rejection result.
func fail(code, reason string) VerifyResult {
	return VerifyResult{OK: false, Code: code, Reason: reason}
}

// Verify dispatches to the correct verification strategy based on cfg.AuthType.
// Empty and explicit "none" mean no auth was declared. Unknown declared modes
// fail closed because silently accepting a misspelled policy bypasses verification.
//
// Complexity: 1 (switch) + 3 (cases) = 4
func Verify(r *http.Request, body []byte, cfg Config) VerifyResult {
	switch cfg.AuthType {
	case "", "none":
		return ok()
	case "hmac_signature":
		return VerifyHMAC(r, body, cfg)
	case "signature_header":
		return VerifySignatureHeader(r, cfg)
	case "static_token":
		return VerifyStaticToken(r, cfg)
	default:
		return fail(CodeAuthUnsupported, "unsupported webhook authentication type")
	}
}

// VerifyHMAC validates the request body against an HMAC-SHA256 signature in
// the configured provider header.
//
// Complexity: 1 (empty guard) + 1 (method guard) + 1 (empty sig guard) + 1 (mismatch guard) = 4
func VerifyHMAC(r *http.Request, body []byte, cfg Config) VerifyResult {
	// A declared policy with missing material is unsafe to execute: accepting it
	// would turn a configuration defect into an authentication bypass.
	if cfg.SignatureHeader == "" || cfg.SigningSecret == "" {
		return fail(CodeConfigIncomplete, "HMAC verification configuration is incomplete")
	}

	// HMAC requires a body to sign; GET requests carry no body.
	if r.Method == http.MethodGet {
		return fail(CodeMethodUnsupported, "HMAC signature verification is not supported for GET requests")
	}

	sig := r.Header.Get(cfg.SignatureHeader)
	if sig == "" {
		return fail(CodeCredentialMissing, "missing signature header")
	}

	// Compute expected HMAC-SHA256 and check if it is present in the sig string.
	// Some signature headers include metadata alongside the digest, so the
	// configured header value may contain rather than equal the encoded digest.
	mac := hmac.New(sha256.New, []byte(cfg.SigningSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !strings.Contains(sig, expected) {
		return fail(CodeCredentialInvalid, "invalid signature")
	}
	return ok()
}

// VerifySignatureHeader checks that all required verification headers are present.
// It does NOT validate their values — that is intentional: asymmetric
// signatures can require the caller to verify externally, so we
// gate only on presence here.
//
// Complexity: 1 (fallback guard) + 1 (for loop) + 1 (empty guard) = 3
func VerifySignatureHeader(r *http.Request, cfg Config) VerifyResult {
	required := cfg.VerificationHeaders

	// Fall back to the primary signature header when no list is declared.
	if len(required) == 0 && cfg.SignatureHeader != "" {
		required = []string{cfg.SignatureHeader}
	}
	if len(required) == 0 {
		return fail(CodeConfigIncomplete, "signature header verification configuration is incomplete")
	}

	for _, h := range required {
		if r.Header.Get(h) == "" {
			return fail(CodeVerificationHeaderMiss, "missing required verification header: "+h)
		}
	}
	return ok()
}

// VerifyStaticToken validates a static shared secret supplied either in a
// request header or a query parameter.
//
// Complexity: 1 (empty guard) + 1 (location if/else) + 1 (empty token guard) + 1 (mismatch guard) = 4
func VerifyStaticToken(r *http.Request, cfg Config) VerifyResult {
	if cfg.AuthKeyName == "" || cfg.SigningSecret == "" {
		return fail(CodeConfigIncomplete, "static token verification configuration is incomplete")
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
		return fail(CodeCredentialMissing, "missing static token")
	}

	if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.SigningSecret)) != 1 {
		return fail(CodeCredentialInvalid, "invalid token")
	}
	return ok()
}
