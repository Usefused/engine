package testcontract

import "github.com/Usefused/engine/internal/shared/signaturepolicy"

// AuthenticatedSignature returns an isolated timestamped recipe used across verifier and ingress tests.
func AuthenticatedSignature() signaturepolicy.Config {
	return mustDecode[signaturepolicy.Config](authenticatedSignature)
}

const authenticatedSignature = `{
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
