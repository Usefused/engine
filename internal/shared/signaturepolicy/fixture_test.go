package signaturepolicy_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Usefused/engine/internal/shared/signaturepolicy"
)

// TestSignatureContractFixturesValidate keeps provider-neutral signature recipes executable without provider-name dispatch.
// TestSignatureContractFixturesValidate covers each generic verification shape
// so the inventory does not depend on provider-labelled filenames.
func TestSignatureContractFixturesValidate(t *testing.T) {
	for _, name := range []string{"v1_generic_header.json", "v1_raw_body_callback_signature.json", "v1_url_form_signature.json", "v1_conditional_challenge_jwt.json"} {
		body, err := os.ReadFile("../../../../contract-fixtures/signature/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var policy signaturepolicy.Config
		if err := json.Unmarshal(body, &policy); err != nil {
			t.Fatal(err)
		}
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
