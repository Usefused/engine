package signaturepolicy

import (
	"encoding/json"
	"testing"
)

// structuredFixture decodes fresh pointers so mutations cannot alter another test's signed input selector.
func structuredFixture(t *testing.T) Config {
	t.Helper()
	var policy Config
	// Fixture decoding must succeed before negative admission cases can be trusted.
	if err := json.Unmarshal([]byte(structuredFixtureJSON), &policy); err != nil {
		t.Fatal(err)
	}
	return policy
}

// TestStructuredAdmission protects version negotiation and exact timestamp coverage in both policy owners.
func TestStructuredAdmission(t *testing.T) {
	cases := []struct {
		name     string
		mutation string
		reject   bool
	}{
		{"valid", "", false}, {"v1", "v1", true}, {"v2", "v2", true},
		{"wrong timestamp key", "key", true}, {"wrong timestamp separator", "separator", true},
		{"two timestamp sources", "both", true}, {"query extraction", "query", true},
		{"unsupported delimiter", "delimiter", true}, {"ignored source", "ignored", true},
		{"missing source", "missing", true}, {"case-insensitive header", "case", false},
		{"empty selector", "empty", true},
	}
	for _, tc := range cases {
		// Every case isolates one policy admission invariant.
		t.Run(tc.name, func(t *testing.T) {
			p := structuredFixture(t)
			s := p.Rules[0].Verification.Signature
			// Mutations distinguish malformed contracts from the positive and case-folding controls.
			switch tc.mutation {
			case "v1":
				p.Version = 1
			case "v2":
				p.Version = 2
			case "key":
				s.Timestamp.Source.Field.Key = "unsigned"
			case "separator":
				s.Timestamp.Source.Field.Separator = ";"
			case "both":
				s.Timestamp.Header = "Other"
			case "query":
				s.Signature.Location = LocationQuery
			case "delimiter":
				s.Signature.Field.Separator = "|"
			case "ignored":
				s.Components[0].Kind = ComponentRawBody
			case "missing":
				s.Components[0].Source = nil
			case "case":
				s.Timestamp.Source.Name = "stripe-signature"
			case "empty":
				s.Signature.Field.Key = ""
			}
			err := NormalizeAndValidate(&p)
			// Accepting a malformed contract could authenticate different bytes than the provider signed.
			if (err != nil) != tc.reject {
				t.Fatalf("error=%v reject=%v", err, tc.reject)
			}
		})
	}
}

const structuredFixtureJSON = `{
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
