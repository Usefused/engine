package webhookverify_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"github.com/Usefused/engine/internal/engine/webhookverify"
	"github.com/Usefused/engine/internal/testcontract"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// structuredDigest uses independent signing code over immutable provider bytes.
func structuredDigest(stamp, body string) string {
	mac := hmac.New(sha256.New, []byte("test-signing-key"))
	mac.Write([]byte(stamp + "." + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// TestStructuredHeaderVerification exercises real provider headers without synthetic adapter fields.
func TestStructuredHeaderVerification(t *testing.T) {
	const body = `{"type":"invoice.paid"}`
	good := structuredDigest("2000000000", body)
	bad := strings.Repeat("0", 64)
	cases := []struct {
		name, header, body string
		ok                 bool
	}{
		{"valid", "t=2000000000,v1=" + good, body, true},
		{"rotation second", "t=2000000000,v1=" + bad + ",v1=" + good, body, true},
		{"rotation first", "t=2000000000,v1=" + good + ",v1=" + bad, body, true},
		{"unknown version", "v0=" + bad + ", t=2000000000, v1=" + good, body, true},
		{"reordered", "v1=" + good + ",t=2000000000", body, true},
		{"old boundary", "t=1999999700,v1=" + structuredDigest("1999999700", body), body, true},
		{"future boundary", "t=2000000030,v1=" + structuredDigest("2000000030", body), body, true},
		{"stale", "t=1999999699,v1=" + structuredDigest("1999999699", body), body, false},
		{"future", "t=2000000031,v1=" + structuredDigest("2000000031", body), body, false},
		{"tampered body", "t=2000000000,v1=" + good, body + " ", false},
		{"tampered timestamp", "t=2000000001,v1=" + good, body, false},
		{"duplicate timestamp", "t=2000000000,t=2000000000,v1=" + good, body, false},
		{"missing timestamp", "v1=" + good, body, false},
		{"noncanonical timestamp", "t=02000000000,v1=" + structuredDigest("02000000000", body), body, false},
		{"missing digest", "t=2000000000,v0=" + good, body, false},
		{"all wrong", "t=2000000000,v1=" + bad + ",v1=" + bad, body, false},
		{"substring", "t=2000000000,v1=x" + good + "x", body, false},
		{"empty member", "t=2000000000,,v1=" + good, body, false},
		{"malformed member", "t=2000000000,v1=" + good + ",broken", body, false},
		{"empty value", "t=2000000000,v1=" + good + ",x=", body, false},
		{"quoted", "t=2000000000,v1=\"" + good + "\"", body, false},
		{"too many candidates", "t=2000000000" + strings.Repeat(",v1="+good, 9), body, false},
		{"candidate bound", "t=2000000000" + strings.Repeat(",v1="+good, 8), body, true},
		{"too many fields", "t=2000000000,v1=" + good + strings.Repeat(",x=y", 31), body, false},
		{"oversized", "t=2000000000,v1=" + good + ",x=" + strings.Repeat("a", 4096), body, false},
	}
	for _, tc := range cases {
		// Each request reaches the public verifier with exactly the original header and body.
		t.Run(tc.name, func(t *testing.T) {
			p := testcontract.StructuredSignature()
			req := httptest.NewRequest("POST", "https://engine.example/webhook/test", strings.NewReader(tc.body))
			req.Header.Set("Stripe-Signature", tc.header)
			input := webhookverify.PolicyInput{Request: req, RawBody: []byte(tc.body), Now: time.Unix(2000000000, 0), Resolve: fixtureResolver("test-signing-key")}
			result := webhookverify.VerifyPolicy(context.Background(), &p, input)
			// A positive digest match cannot bypass framing, scalar ambiguity or freshness checks.
			if result.OK != tc.ok {
				t.Fatalf("result=%+v want=%v", result, tc.ok)
			}
		})
	}
}

// TestStructuredHeaderIsProviderNeutral proves alternate header names, keys and delimiters need no provider branch.
func TestStructuredHeaderIsProviderNeutral(t *testing.T) {
	p := testcontract.StructuredSignature()
	s := p.Rules[0].Verification.Signature
	s.Signature.Name = "X-Event-Proof"
	s.Signature.Field.Key = "digest"
	s.Signature.Field.Separator = ";"
	s.Signature.Field.Assignment = ":"
	s.Timestamp.Source.Name = "X-Event-Proof"
	s.Timestamp.Source.Field.Key = "created"
	s.Timestamp.Source.Field.Separator = ";"
	s.Timestamp.Source.Field.Assignment = ":"
	s.Components[0].Source = s.Timestamp.Source
	req := httptest.NewRequest("POST", "https://engine.example/webhook/test", nil)
	req.Header.Set("X-Event-Proof", "created:2000000000;digest:"+structuredDigest("2000000000", "{}"))
	input := webhookverify.PolicyInput{Request: req, RawBody: []byte("{}"), Now: time.Unix(2000000000, 0), Resolve: fixtureResolver("test-signing-key")}
	// The same generic verifier must accept this distinct provider protocol.
	if result := webhookverify.VerifyPolicy(context.Background(), &p, input); !result.OK {
		t.Fatal(result)
	}
	req.Header.Add("X-Event-Proof", req.Header.Get("X-Event-Proof"))
	// Repeated HTTP field lines are rejected rather than combined according to an intermediary's parser.
	if result := webhookverify.VerifyPolicy(context.Background(), &p, input); result.OK {
		t.Fatal("duplicate header accepted")
	}
	req.Header.Del("X-Event-Proof")
	req.Header.Set("X-Fused-Stripe-Timestamp", "2000000000")
	req.Header.Set("X-Fused-Stripe-V1", structuredDigest("2000000000", "{}"))
	// Caller-supplied synthetic headers cannot substitute for the declared provider credential.
	if result := webhookverify.VerifyPolicy(context.Background(), &p, input); result.OK {
		t.Fatal("synthetic headers accepted")
	}
}
