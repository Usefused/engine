package signaturepolicy_test

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/signaturepolicy"
	"github.com/Usefused/engine/internal/testcontract"
)

// TestSignatureContractsValidate covers each generic verification shape
// from Engine-owned contracts without a control-plane repository checkout.
func TestSignatureContractsValidate(t *testing.T) {
	for _, name := range []string{"generic_header", "raw_body_callback", "url_form", "conditional_challenge_jwt"} {
		policy := testcontract.SignaturePolicy(name)
		if err := signaturepolicy.Validate(&policy); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestSignatureV1RejectsUnsupportedJWTAlgorithm(t *testing.T) {
	policy := signaturepolicy.Config{Version: 1, Rules: []signaturepolicy.Rule{{
		Name: "event", Kind: signaturepolicy.RuleEvent, Verification: signaturepolicy.Verification{
			Kind: signaturepolicy.VerificationJWT, JWT: &signaturepolicy.JWTVerification{
				SecretRef: "${bucket.secret.key}", Token: signaturepolicy.ValueSource{Location: signaturepolicy.LocationHeader, Name: "Authorization"}, Algorithms: []string{"RS256"},
			},
		},
	}}}
	if err := signaturepolicy.Validate(&policy); err == nil {
		t.Fatal("expected non-HMAC JWT algorithm rejection")
	}
}
