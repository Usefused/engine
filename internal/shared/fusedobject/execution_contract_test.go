package fusedobject

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestExecutionContractEnvelopeValidation(t *testing.T) {
	tests := []struct {
		name     string
		envelope ExecutionContractEnvelope
		wantErr  string
	}{
		{name: "supported", envelope: EngineExecutionContractSupport()},
		{name: "missing version", envelope: ExecutionContractEnvelope{RequiredCapabilities: []string{}}, wantErr: ExecutionContractReasonUnsupportedVersion},
		{name: "future version", envelope: ExecutionContractEnvelope{ContractVersion: 3, RequiredCapabilities: []string{}}, wantErr: ExecutionContractReasonUnsupportedVersion},
		{name: "missing capabilities", envelope: ExecutionContractEnvelope{ContractVersion: CurrentExecutionContractVersion}, wantErr: ExecutionContractReasonMissingCapabilities},
		{name: "empty capability", envelope: ExecutionContractEnvelope{ContractVersion: CurrentExecutionContractVersion, RequiredCapabilities: []string{""}}, wantErr: ExecutionContractReasonUnsupportedCapability},
		{name: "unknown capability", envelope: ExecutionContractEnvelope{ContractVersion: CurrentExecutionContractVersion, RequiredCapabilities: []string{"http.future.v1"}}, wantErr: ExecutionContractReasonUnsupportedCapability},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateExecutionContractEnvelope(test.envelope)
			if test.wantErr == "" && err != nil {
				t.Fatalf("ValidateExecutionContractEnvelope() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("ValidateExecutionContractEnvelope() error = %v, want containing %q", err, test.wantErr)
			}
			if test.wantErr != "" {
				var compatibilityErr *ExecutionContractCompatibilityError
				if !errors.As(err, &compatibilityErr) || !strings.Contains(err.Error(), ExecutionCapabilityRequiredCode) {
					t.Fatalf("error = %#v, want stable execution compatibility classification", err)
				}
			}
		})
	}
}

func TestCanonicalExecutionContractEnvelopeSortsAndDeduplicates(t *testing.T) {
	envelope, err := CanonicalExecutionContractEnvelope(ExecutionContractEnvelope{
		ContractVersion: CurrentExecutionContractVersion,
		RequiredCapabilities: []string{
			ExecutionCapabilityRetryPolicyV3,
			ExecutionCapabilityHTTPDigestV1,
			ExecutionCapabilityRetryPolicyV3,
		},
	})
	if err != nil {
		t.Fatalf("CanonicalExecutionContractEnvelope: %v", err)
	}
	want := []string{ExecutionCapabilityHTTPDigestV1, ExecutionCapabilityRetryPolicyV3}
	if !reflect.DeepEqual(envelope.RequiredCapabilities, want) {
		t.Fatalf("canonical capabilities = %#v, want %#v", envelope.RequiredCapabilities, want)
	}
}

func TestCanonicalExecutionContractEnvelopePreservesExplicitEmptyArray(t *testing.T) {
	envelope, err := CanonicalExecutionContractEnvelope(ExecutionContractEnvelope{
		ContractVersion: CurrentExecutionContractVersion, RequiredCapabilities: []string{},
	})
	if err != nil {
		t.Fatalf("CanonicalExecutionContractEnvelope: %v", err)
	}
	if envelope.RequiredCapabilities == nil || len(envelope.RequiredCapabilities) != 0 {
		t.Fatalf("canonical empty capabilities = %#v, want explicit empty array", envelope.RequiredCapabilities)
	}
}

// TestEngineExecutionContractSupportEncodesExplicitCapabilities freezes the
// exact GraphQL negotiation set advertised to Registry.
func TestEngineExecutionContractSupportEncodesExplicitCapabilities(t *testing.T) {
	payload, err := json.Marshal(EngineExecutionContractSupport())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"contract_version":2,"required_capabilities":["auth.http.digest.v1","auth.oauth1.signature.v1","auth.oauth2.multiflow.v1","auth.oauth2.token_request_media.v1","auth.security.server_selection.v1","connection.resource_discovery.v1","http.methods.extensible.v1","http.multipart.positional.v1","http.parameter.querystring.v1","http.parameter.serialization.v1","http.parameter.serialization.v2","http.request.alternatives.v1","http.response.contracts.v1","http.sequential_media.v1","http.server.named.v1","http.server.precedence.v1","http.upload.workflow.v1","json.schema.contract.v1","pagination.composable.v3","quota.multidimensional.v3","retry.policy.v3","webhook.signature.recipes.v1"]}`
	if string(payload) != want {
		t.Fatalf("support JSON = %s", payload)
	}
}
