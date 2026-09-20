package webhookverify_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/webhookverify"
	"github.com/Usefused/engine/internal/testcontract"
)

// timestampedInput signs independent bytes so mutation cases test authentication rather than a mirrored verifier.
func timestampedInput(body, timestamp string, now time.Time) webhookverify.PolicyInput {
	mac := hmac.New(sha256.New, []byte("test-signing-key"))
	_, _ = mac.Write([]byte("v0:" + timestamp + ":" + body))
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", nil)
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return webhookverify.PolicyInput{Request: req, RawBody: []byte(body), Now: now, Resolve: fixtureResolver("test-signing-key")}
}

// TestTimestampedSignatureRejectsTamperingAndReplay exercises protocol ambiguity and freshness failures.
func TestTimestampedSignatureRejectsTamperingAndReplay(t *testing.T) {
	now := time.Unix(2000000000, 0)
	cases := []struct {
		name   string
		delta  time.Duration
		mutate func(*webhookverify.PolicyInput)
		ok     bool
	}{
		{name: "valid", ok: true}, {name: "old boundary", delta: -5 * time.Minute, ok: true}, {name: "future boundary", delta: 5 * time.Minute, ok: true},
		{name: "stale", delta: -301 * time.Second}, {name: "future", delta: 301 * time.Second},
		// Mutations occur after signing and must never be repaired by normalization.
		{name: "body changed", mutate: func(i *webhookverify.PolicyInput) { i.RawBody = []byte(`{"event":"different"}`) }},
		{name: "missing prefix", mutate: func(i *webhookverify.PolicyInput) {
			i.Request.Header.Set("X-Slack-Signature", strings.TrimPrefix(i.Request.Header.Get("X-Slack-Signature"), "v0="))
		}},
		{name: "wrong digest", mutate: func(i *webhookverify.PolicyInput) { i.Request.Header.Set("X-Slack-Signature", "v0=wrong") }},
		{name: "duplicate signature", mutate: func(i *webhookverify.PolicyInput) {
			i.Request.Header.Add("X-Slack-Signature", i.Request.Header.Get("X-Slack-Signature"))
		}},
		{name: "duplicate timestamp", mutate: func(i *webhookverify.PolicyInput) {
			i.Request.Header.Add("X-Slack-Request-Timestamp", i.Request.Header.Get("X-Slack-Request-Timestamp"))
		}},
		{name: "missing signature", mutate: func(i *webhookverify.PolicyInput) { i.Request.Header.Del("X-Slack-Signature") }},
		{name: "missing timestamp", mutate: func(i *webhookverify.PolicyInput) { i.Request.Header.Del("X-Slack-Request-Timestamp") }},
		{name: "malformed timestamp", mutate: func(i *webhookverify.PolicyInput) { i.Request.Header.Set("X-Slack-Request-Timestamp", "+2000000000") }},
	}
	for _, tc := range cases {
		// Each case receives an isolated signed request to avoid header mutation leaking between tests.
		t.Run(tc.name, func(t *testing.T) {
			policy := testcontract.AuthenticatedSignature()
			input := timestampedInput(`{"event":"test"}`, strconv.FormatInt(now.Add(tc.delta).Unix(), 10), now)
			// A nil mutation intentionally preserves the positive and time-boundary controls.
			if tc.mutate != nil {
				tc.mutate(&input)
			}
			result := webhookverify.VerifyPolicy(context.Background(), &policy, input)
			// Event rules must never turn into an HTTP challenge response.
			if result.OK != tc.ok || result.Code == webhookverify.CodeChallengeResponded {
				t.Fatalf("result = %#v, want success %v", result, tc.ok)
			}
		})
	}
}

// TestChallengeRequiresValidSignature proves public challenge responses cannot bypass authentication.
func TestChallengeRequiresValidSignature(t *testing.T) {
	policy := testcontract.AuthenticatedSignature()
	now := time.Unix(2000000000, 0)
	input := timestampedInput(`{"type":"url_verification","challenge":"proof"}`, "2000000000", now)
	result := webhookverify.VerifyPolicy(context.Background(), &policy, input)
	// The authenticated challenge returns provider-compatible JSON but no event result.
	if !result.OK || result.Code != webhookverify.CodeChallengeResponded || string(result.ChallengeBody) != `{"challenge":"proof"}` {
		t.Fatalf("challenge: %#v", result)
	}
	input.Request.Header.Del("X-Slack-Signature")
	result = webhookverify.VerifyPolicy(context.Background(), &policy, input)
	// An attacker must not obtain a challenge response by matching the predicate alone.
	if result.OK || len(result.ChallengeBody) != 0 {
		t.Fatalf("unsigned challenge: %#v", result)
	}
}

// TestNilPolicyFailsClosed prevents absent policy input from panicking at the public boundary.
func TestNilPolicyFailsClosed(t *testing.T) {
	result := webhookverify.VerifyPolicy(context.Background(), nil, webhookverify.PolicyInput{})
	// Missing configuration is a policy error, never implicit unsigned delivery.
	if result.OK || result.Code != webhookverify.CodePolicyInvalid {
		t.Fatalf("result: %#v", result)
	}
}
