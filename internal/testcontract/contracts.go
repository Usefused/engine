// Package testcontract provides the small, Engine-owned contracts shared by
// tests in multiple Engine packages. Cross-repository golden fixtures belong
// to the integration workspace and must not become a standalone Engine CI
// dependency.
package testcontract

import (
	"encoding/json"
	"fmt"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/signaturepolicy"
	"github.com/Usefused/engine/internal/shared/workflowcontract"
)

const retryV3 = `{
  "version": 3,
  "rules": [{
    "predicates": {
      "methods": ["GET", "HEAD"],
      "operation_kinds": ["read", "query"],
      "statuses": [{"min": 429, "max": 429}, {"min": 500, "max": 599}],
      "errors": [],
      "body_replayability": "any",
      "idempotency_key": {"requirement": "any"},
      "required_provider_headers": []
    },
    "action": {
      "max_attempts": 3,
      "max_elapsed_ms": 30000,
      "backoff": {"strategy": "exponential", "base_delay_ms": 250, "max_delay_ms": 5000, "jitter_ms": 100},
      "retry_after_headers": [{"name": "Retry-After", "formats": ["delta_seconds", "http_date"], "max_delay_ms": 10000}]
    }
  }]
}`

const uploadWorkflow = `{
  "version": 1,
  "accepted_media_types": ["application/octet-stream", "image/png"],
  "max_size_bytes": 5368709120,
  "modes": [
    {"kind":"simple","steps":[{"kind":"transfer","method":"POST","url":{"kind":"declared_path","path":"/upload/v1/files"},"body":"media","success_statuses":[{"min":200,"max":299}],"continue_statuses":[]}]},
    {"kind":"multipart","steps":[{"kind":"transfer","method":"POST","url":{"kind":"declared_path","path":"/upload/v1/files"},"body":"multipart","success_statuses":[{"min":200,"max":299}],"continue_statuses":[]}]},
    {"kind":"resumable","steps":[
      {"kind":"initiate","method":"POST","url":{"kind":"declared_path","path":"/resumable/upload/v1/files"},"body":"metadata","success_statuses":[{"min":200,"max":201}],"continue_statuses":[]},
      {"kind":"transfer","method":"PUT","url":{"kind":"response_header","header_name":"Location","allowed_origins":["https://uploads.example.test"]},"body":"media","chunking":{"default_size_bytes":8388608,"size_multiple_bytes":262144,"max_size_bytes":268435456},"success_statuses":[{"min":200,"max":299}],"continue_statuses":[{"min":308,"max":308}]}
    ]}
  ]
}`

const rawBodyCallbackSignature = `{"version":1,"rules":[{"name":"event","kind":"event","predicates":[],"verification":{"kind":"signature","signature":{"secret_ref":"${bucket.secret.webhook_signing_key}","signature":{"location":"header","name":"X-Callback-Signature"},"components":[{"kind":"raw_body","names":[]},{"kind":"exact_callback_url","names":[]}],"algorithm":"hmac_sha1","encoding":"base64","comparison":"constant_time","component_separator":""}}}]}`

const genericHeaderSignature = `{"version":1,"rules":[{"name":"event","kind":"event","predicates":[],"verification":{"kind":"signature","signature":{"secret_ref":"${bucket.secret.webhook_signing_key}","signature":{"location":"header","name":"X-Webhook-Signature"},"components":[{"kind":"raw_body","names":[]}],"algorithm":"hmac_sha256","encoding":"hex","comparison":"constant_time","component_separator":""}}}]}`

const urlFormSignature = `{"version":1,"rules":[{"name":"event","kind":"event","predicates":[],"verification":{"kind":"signature","signature":{"secret_ref":"${bucket.secret.webhook_signing_key}","signature":{"location":"header","name":"X-Webhook-Signature"},"components":[{"kind":"exact_callback_url","names":[]},{"kind":"sorted_form","names":[],"join":"concat_name_value"}],"algorithm":"hmac_sha1","encoding":"base64","comparison":"constant_time","component_separator":""}}}]}`

const conditionalChallengeJWT = `{"version":1,"rules":[{"name":"challenge","kind":"challenge","predicates":[{"source":{"location":"body","path":"$.challenge"},"operator":"present"}],"verification":{"kind":"challenge_response","challenge":{"value":{"location":"body","path":"$.challenge"},"body_field":"challenge","status_code":200}}},{"name":"event","kind":"event","predicates":[],"verification":{"kind":"jwt","jwt":{"secret_ref":"${bucket.secret.webhook_signing_key}","token":{"location":"header","name":"Authorization"},"algorithms":["HS256"],"audience":"webhook","clock_skew_ms":30000}}}]}`

// TransportContract groups the independent auth, security-alternative, and
// server values that Engine validates at its local snapshot boundary.
type TransportContract struct {
	AuthConfig           fusedobject.AuthConfig
	OAuthAuthConfig      fusedobject.AuthConfig
	SecurityRequirements authrouting.Requirements
	Server               fusedobject.Server
}

// RetryV3JSON returns a fresh payload so one test cannot mutate another test's
// canonical policy while checking publish and override round trips.
func RetryV3JSON() []byte {
	return []byte(retryV3)
}

// ResourceDiscovery returns only execution-relevant discovery data; provider
// research and acceptance examples intentionally remain outside this repo.
func ResourceDiscovery() fusedobject.ResourceDiscoveryConfig {
	return fusedobject.ResourceDiscoveryConfig{
		Version: 1, Stage: "post_auth", OperationID: "listResources",
		IDPath: "$[*].id", NamePath: "$[*].name",
		BaseURLTemplate: "https://{id}.api.example.test", ResourceType: "project",
		AutoRun: "after_oauth_callback", Lifecycle: "authoritative",
		AllowedHosts: []string{"*.api.example.test"},
	}
}

// Transport returns a fresh provider-neutral contract so Engine transport
// tests exercise wire decisions without reading a control-plane repository.
func Transport() TransportContract {
	return mustDecode[TransportContract](`{
      "AuthConfig":{"name":"emptyPasswordBasic","type":"http","scheme":"basic","basic_password_mode":"empty"},
      "OAuthAuthConfig":{"name":"multiFlowOAuth","type":"oauth2","token_endpoint_auth_method":"client_secret_basic","pkce_required":true,"scopes_delimiter":"comma","oauth2_flows":{"authorizationCode":{"authorization_url":"https://auth.example.test/authorize","token_url":"https://auth.example.test/token","scopes":{"records:read":"Read records"}}}},
      "SecurityRequirements":[{"schemes":[{"scheme":"multiFlowOAuth","scopes":["records:read"]},{"scheme":"clientCertificate","scopes":[]}],"server_selection":{"scheme":"clientCertificate","server_url":"https://mtls.example.test/v1"}},{"schemes":[{"scheme":"emptyPasswordBasic","scopes":[]}]},{"schemes":[]}],
      "Server":{"url":"https://{tenant}.example.test/{api-version}","name":"tenant","environment":"production","variables":[{"name":"tenant","enum":["example","sandbox"],"required":true},{"name":"api-version","default":"v1","enum":["v1","v2"],"required":false}]}
    }`)
}

// UploadWorkflow returns a newly decoded workflow because runtime tests mutate
// status and origin branches when proving fail-closed behavior.
func UploadWorkflow() workflowcontract.UploadWorkflow {
	return mustDecode[workflowcontract.UploadWorkflow](uploadWorkflow)
}

// SignaturePolicy selects a semantic recipe rather than a provider or fixture
// filename, keeping webhook execution independent of import research.
func SignaturePolicy(name string) signaturepolicy.Config {
	switch name {
	case "generic_header":
		return mustDecode[signaturepolicy.Config](genericHeaderSignature)
	case "raw_body_callback":
		return mustDecode[signaturepolicy.Config](rawBodyCallbackSignature)
	case "url_form":
		return mustDecode[signaturepolicy.Config](urlFormSignature)
	case "conditional_challenge_jwt":
		return mustDecode[signaturepolicy.Config](conditionalChallengeJWT)
	default:
		panic(fmt.Sprintf("unknown Engine test signature contract %q", name))
	}
}

// mustDecode centralizes strict contract construction so malformed test data
// fails before any runtime assertion can accidentally exercise a zero value.
func mustDecode[T any](raw string) T {
	var value T
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		panic(err)
	}
	return value
}
