package fusedobject

import (
	"errors"
	"fmt"
	"sort"
)

const CurrentExecutionContractVersion = 2

const (
	ExecutionCapabilityHTTPParameterSerializationV1  = "http.parameter.serialization.v1"
	ExecutionCapabilityHTTPMethodsExtensibleV1       = "http.methods.extensible.v1"
	ExecutionCapabilityHTTPParameterQueryStringV1    = "http.parameter.querystring.v1"
	ExecutionCapabilityHTTPParameterSerializationV2  = "http.parameter.serialization.v2"
	ExecutionCapabilityHTTPServerNamedV1             = "http.server.named.v1"
	ExecutionCapabilityHTTPSequentialMediaV1         = "http.sequential_media.v1"
	ExecutionCapabilityHTTPMultipartPositionalV1     = "http.multipart.positional.v1"
	ExecutionCapabilityHTTPServerPrecedenceV1        = "http.server.precedence.v1"
	ExecutionCapabilityJSONSchemaContractV1          = "json.schema.contract.v1"
	ExecutionCapabilityJSONSchemaSharedDefinitionsV1 = "json.schema.shared_definitions.v1"
	ExecutionCapabilityHTTPRequestAlternativesV1     = "http.request.alternatives.v1"
	ExecutionCapabilityHTTPResponseContractsV1       = "http.response.contracts.v1"
	ExecutionCapabilityOAuth2MultiFlowV1             = "auth.oauth2.multiflow.v1"
	ExecutionCapabilityOAuth2RefreshTokenRequiredV1  = "auth.oauth2.refresh_token_required.v1"
	ExecutionCapabilityOAuth2TokenRequestMediaV1     = "auth.oauth2.token_request_media.v1"
	ExecutionCapabilityOAuth1SignatureV1             = "auth.oauth1.signature.v1"
	ExecutionCapabilityHTTPDigestV1                  = "auth.http.digest.v1"
	ExecutionCapabilitySecurityServerSelectionV1     = "auth.security.server_selection.v1"
	ExecutionCapabilityPaginationComposableV3        = "pagination.composable.v3"
	ExecutionCapabilityQuotaMultidimensionalV3       = "quota.multidimensional.v3"
	ExecutionCapabilityRetryPolicyV3                 = "retry.policy.v3"
	ExecutionCapabilityWebhookSignatureRecipesV1     = "webhook.signature.recipes.v1"
	ExecutionCapabilityHTTPUploadWorkflowV1          = "http.upload.workflow.v1"
	ExecutionCapabilityConnectionResourceDiscoveryV1 = "connection.resource_discovery.v1"
)

var supportedExecutionCapabilityOrder = []string{
	ExecutionCapabilityHTTPDigestV1,
	ExecutionCapabilityOAuth1SignatureV1,
	ExecutionCapabilityOAuth2MultiFlowV1,
	ExecutionCapabilityOAuth2RefreshTokenRequiredV1,
	ExecutionCapabilityOAuth2TokenRequestMediaV1,
	ExecutionCapabilitySecurityServerSelectionV1,
	ExecutionCapabilityConnectionResourceDiscoveryV1,
	ExecutionCapabilityHTTPMethodsExtensibleV1,
	ExecutionCapabilityHTTPMultipartPositionalV1,
	ExecutionCapabilityHTTPParameterQueryStringV1,
	ExecutionCapabilityHTTPParameterSerializationV1,
	ExecutionCapabilityHTTPParameterSerializationV2,
	ExecutionCapabilityHTTPRequestAlternativesV1,
	ExecutionCapabilityHTTPResponseContractsV1,
	ExecutionCapabilityHTTPSequentialMediaV1,
	ExecutionCapabilityHTTPServerNamedV1,
	ExecutionCapabilityHTTPServerPrecedenceV1,
	ExecutionCapabilityHTTPUploadWorkflowV1,
	ExecutionCapabilityJSONSchemaContractV1,
	ExecutionCapabilityJSONSchemaSharedDefinitionsV1,
	ExecutionCapabilityPaginationComposableV3,
	ExecutionCapabilityQuotaMultidimensionalV3,
	ExecutionCapabilityRetryPolicyV3,
	ExecutionCapabilityWebhookSignatureRecipesV1,
}

var supportedExecutionCapabilities = executionCapabilitySet(supportedExecutionCapabilityOrder)

const (
	ExecutionCapabilityRequiredCode = "execution_capability_required"

	ExecutionContractReasonUnsupportedVersion    = "unsupported_version"
	ExecutionContractReasonMissingCapabilities   = "missing_capabilities"
	ExecutionContractReasonUnsupportedCapability = "unsupported_capability"
)

// ExecutionContractCompatibilityError gives every runtime boundary one stable
// classification without copying an untrusted capability name into telemetry.
// The bounded version and count are enough to debug rollout mismatches.
type ExecutionContractCompatibilityError struct {
	Reason          string
	ContractVersion int
	CapabilityCount int
}

func (e *ExecutionContractCompatibilityError) Error() string {
	return fmt.Sprintf("%s: %s (contract_version=%d required_capabilities=%d)",
		ExecutionCapabilityRequiredCode, e.Reason, e.ContractVersion, e.CapabilityCount)
}

// ExecutionContractEnvelope separates wire-shape compatibility from optional
// documentation fields. Required capabilities must be understood before the
// Engine can safely execute the surrounding contract.
type ExecutionContractEnvelope struct {
	ContractVersion      int      `json:"contract_version"`
	RequiredCapabilities []string `json:"required_capabilities"`
}

// EngineExecutionContractSupport is the single source used for both GraphQL
// negotiation and local validation. A fresh empty slice is intentional because
// GraphQL must receive the explicit supported set rather than infer semantics
// from the contract version or an omitted capability array.
func EngineExecutionContractSupport() ExecutionContractEnvelope {
	return ExecutionContractEnvelope{
		ContractVersion:      CurrentExecutionContractVersion,
		RequiredCapabilities: append([]string(nil), supportedExecutionCapabilityOrder...),
	}
}

func ValidateExecutionContractEnvelope(envelope ExecutionContractEnvelope) error {
	_, err := CanonicalExecutionContractEnvelope(envelope)
	return err
}

// CanonicalExecutionContractEnvelope validates set membership and returns the
// one sorted, duplicate-free representation used by snapshot hashing/storage.
func CanonicalExecutionContractEnvelope(envelope ExecutionContractEnvelope) (ExecutionContractEnvelope, error) {
	if envelope.ContractVersion != CurrentExecutionContractVersion {
		return envelope, newExecutionContractCompatibilityError(envelope, ExecutionContractReasonUnsupportedVersion)
	}
	if envelope.RequiredCapabilities == nil {
		return envelope, newExecutionContractCompatibilityError(envelope, ExecutionContractReasonMissingCapabilities)
	}
	// Preserve an explicit empty array: nil means the envelope was omitted,
	// while [] means the current contract intentionally needs no capabilities.
	capabilities := append([]string{}, envelope.RequiredCapabilities...)
	sort.Strings(capabilities)
	canonical := capabilities[:0]
	for _, capability := range capabilities {
		if _, supported := supportedExecutionCapabilities[capability]; !supported {
			return envelope, newExecutionContractCompatibilityError(envelope, ExecutionContractReasonUnsupportedCapability)
		}
		if len(canonical) == 0 || canonical[len(canonical)-1] != capability {
			canonical = append(canonical, capability)
		}
	}
	envelope.RequiredCapabilities = canonical
	return envelope, nil
}

func executionCapabilitySet(capabilities []string) map[string]struct{} {
	result := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		result[capability] = struct{}{}
	}
	return result
}

func newExecutionContractCompatibilityError(envelope ExecutionContractEnvelope, reason string) error {
	return &ExecutionContractCompatibilityError{
		Reason: reason, ContractVersion: envelope.ContractVersion, CapabilityCount: len(envelope.RequiredCapabilities),
	}
}

func ExecutionContractCompatibilityDetails(err error) (*ExecutionContractCompatibilityError, bool) {
	var compatibilityErr *ExecutionContractCompatibilityError
	ok := errors.As(err, &compatibilityErr)
	return compatibilityErr, ok
}
