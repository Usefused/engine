package signaturepolicy

import (
	"encoding/json"
	"testing"
)

// authenticatedFixture decodes fresh policy state so mutation tests cannot share pointer aliases.
func authenticatedFixture(t *testing.T) Config {
	t.Helper()
	var policy Config
	// A malformed fixture must stop before its zero values can accidentally satisfy a rejection test.
	if err := json.Unmarshal([]byte(authenticatedFixtureJSON), &policy); err != nil {
		t.Fatal(err)
	}
	return policy
}

// TestAuthenticatedPolicyAdmission prevents weaker engines or unsigned freshness inputs from accepting v2 behavior.
func TestAuthenticatedPolicyAdmission(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		reject bool
	}{
		{name: "valid"},
		// Each malformed policy removes one independent security invariant.
		{name: "v1 extension", mutate: func(p *Config) { p.Version = 1 }, reject: true},
		{name: "unsigned timestamp", mutate: func(p *Config) { p.Rules[0].Verification.Signature.Timestamp.Header = "X-Unsigned" }, reject: true},
		{name: "unbounded age", mutate: func(p *Config) { p.Rules[0].Verification.Signature.Timestamp.MaxAgeMs = 300001 }, reject: true},
		{name: "negative skew", mutate: func(p *Config) { p.Rules[0].Verification.Signature.Timestamp.MaxFutureMs = -1 }, reject: true},
		{name: "unsigned response", mutate: func(p *Config) {
			p.Rules[0].Verification.Signature.Components = p.Rules[0].Verification.Signature.Components[:2]
		}, reject: true},
		{name: "header response", mutate: func(p *Config) {
			p.Rules[0].Response.Value = ValueSource{Location: LocationHeader, Name: "X-Challenge"}
		}, reject: true},
		{name: "event response", mutate: func(p *Config) { p.Rules[0].Kind = RuleEvent }, reject: true},
		{name: "empty constant", mutate: func(p *Config) { p.Rules[0].Verification.Signature.Components[0].Value = "" }, reject: true},
	}
	for _, tc := range cases {
		// Subtests report the specific invariant whose admission changed.
		t.Run(tc.name, func(t *testing.T) {
			p := authenticatedFixture(t)
			// The positive control intentionally leaves the reviewed policy intact.
			if tc.mutate != nil {
				tc.mutate(&p)
			}
			err := NormalizeAndValidate(&p)
			// Validation must agree at both the import and runtime boundaries.
			if (err != nil) != tc.reject {
				t.Fatalf("error = %v, reject = %v", err, tc.reject)
			}
		})
	}
}

// TestNormalizePreservesSignedLiteral protects deliberate whitespace in a provider's signed prefix component.
func TestNormalizePreservesSignedLiteral(t *testing.T) {
	p := authenticatedFixture(t)
	p.Rules[0].Verification.Signature.Components[0].Value = " v0 "
	Normalize(&p)
	// Changing these bytes would reject authentic provider requests after persistence.
	if p.Rules[0].Verification.Signature.Components[0].Value != " v0 " {
		t.Fatal("signed literal normalized")
	}
}

const authenticatedFixtureJSON = `{
  "version": 2,
  "rules": [
    {
      "name": "challenge",
      "kind": "challenge",
      "predicates": [
        {
          "source": {
            "location": "body",
            "path": "$.type"
          },
          "operator": "equals",
          "value": "url_verification"
        }
      ],
      "verification": {
        "kind": "signature",
        "signature": {
          "secret_ref": "${bucket.default.secret.test-label}",
          "signature": {
            "location": "header",
            "name": "X-Slack-Signature"
          },
          "components": [
            {
              "kind": "constant",
              "value": "v0",
              "names": []
            },
            {
              "kind": "selected_headers",
              "names": [
                "X-Slack-Request-Timestamp"
              ],
              "join": "concat_values"
            },
            {
              "kind": "raw_body",
              "names": []
            }
          ],
          "algorithm": "hmac_sha256",
          "encoding": "hex",
          "comparison": "constant_time",
          "prefix": "v0=",
          "component_separator": ":",
          "timestamp": {
            "header": "X-Slack-Request-Timestamp",
            "max_age_ms": 300000,
            "max_future_ms": 300000
          }
        }
      },
      "response": {
        "value": {
          "location": "body",
          "path": "$.challenge"
        },
        "body_field": "challenge",
        "status_code": 200
      }
    },
    {
      "name": "event",
      "kind": "event",
      "predicates": [],
      "verification": {
        "kind": "signature",
        "signature": {
          "secret_ref": "${bucket.default.secret.test-label}",
          "signature": {
            "location": "header",
            "name": "X-Slack-Signature"
          },
          "components": [
            {
              "kind": "constant",
              "value": "v0",
              "names": []
            },
            {
              "kind": "selected_headers",
              "names": [
                "X-Slack-Request-Timestamp"
              ],
              "join": "concat_values"
            },
            {
              "kind": "raw_body",
              "names": []
            }
          ],
          "algorithm": "hmac_sha256",
          "encoding": "hex",
          "comparison": "constant_time",
          "prefix": "v0=",
          "component_separator": ":",
          "timestamp": {
            "header": "X-Slack-Request-Timestamp",
            "max_age_ms": 300000,
            "max_future_ms": 300000
          }
        }
      }
    }
  ]
}
`
