package testcontract

import (
	"encoding/json"
	"github.com/Usefused/engine/internal/shared/signaturepolicy"
)

// StructuredSignature returns independent provider-configured selectors for transport and verification tests.
func StructuredSignature() signaturepolicy.Config {
	var policy signaturepolicy.Config
	// A corrupt fixture is a test setup error, not a valid empty policy.
	if err := json.Unmarshal([]byte(structuredSignatureJSON), &policy); err != nil {
		panic(err)
	}
	return policy
}

const structuredSignatureJSON = `{
  "version": 3,
  "rules": [
    {
      "name": "stripe_signed_event",
      "kind": "event",
      "predicates": [],
      "verification": {
        "kind": "signature",
        "signature": {
          "secret_ref": "${bucket.default.secret.signing}",
          "signature": {
            "location": "header",
            "name": "Stripe-Signature",
            "field": {
              "key": "v1",
              "separator": ",",
              "assignment": "="
            }
          },
          "components": [
            {
              "kind": "source",
              "source": {
                "location": "header",
                "name": "Stripe-Signature",
                "field": {
                  "key": "t",
                  "separator": ",",
                  "assignment": "="
                }
              },
              "names": []
            },
            {
              "kind": "raw_body",
              "names": []
            }
          ],
          "algorithm": "hmac_sha256",
          "encoding": "hex",
          "comparison": "constant_time",
          "component_separator": ".",
          "timestamp": {
            "source": {
              "location": "header",
              "name": "Stripe-Signature",
              "field": {
                "key": "t",
                "separator": ",",
                "assignment": "="
              }
            },
            "max_age_ms": 300000,
            "max_future_ms": 30000
          }
        }
      }
    }
  ]
}
`
