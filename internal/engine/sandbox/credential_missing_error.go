package sandbox

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// CredentialMaterialMissingError is the secret-free pre-provider failure for
// one static or OAuth application family that the selected bucket cannot satisfy.
type CredentialMaterialMissingError struct {
	BucketID  uuid.UUID
	ServiceID uuid.UUID
	AuthType  string
	AuthName  string
}

// MarshalJSON exposes only the stable action contract consumed by generated
// SDK runtimes; routing identity and the reconstructed command contain no secret values.
func (e *CredentialMaterialMissingError) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Code      string `json:"code"`
		BucketID  string `json:"bucket_id"`
		ServiceID string `json:"service_id"`
		AuthType  string `json:"auth_type"`
		AuthName  string `json:"auth_name"`
		Command   string `json:"command"`
	}{
		Code: "bucket_credentials_missing", BucketID: e.BucketID.String(), ServiceID: e.ServiceID.String(),
		AuthType: e.AuthType, AuthName: e.AuthName, Command: e.Command(),
	})
}

// Error gives SDK and MCP transports one immediately actionable command while
// keeping all credential values out of the response.
func (e *CredentialMaterialMissingError) Error() string {
	return "bucket_credentials_missing: provider credentials are not configured; run: " + e.Command()
}

// Command returns the canonical interactive CLI remediation without embedding
// a credential value in argv, logs, telemetry, or the error payload.
func (e *CredentialMaterialMissingError) Command() string {
	return fmt.Sprintf(
		"fused-cli secret set %s --bucket %s --type %s --auth-name %s --interactive",
		e.ServiceID.String(), e.BucketID.String(), shellQuoteCredentialArgument(e.AuthType), shellQuoteCredentialArgument(e.AuthName),
	)
}

// missingStaticCredentialError identifies one selected credential family from
// the exact alternative that failed without inspecting or exposing values.
func missingStaticCredentialError(bucketID, serviceID uuid.UUID, alternative store.SecretKeyAlternative) error {
	// Required keys retain deterministic order, so the first owned key names the
	// first credential family the operator can configure before retrying.
	for _, key := range alternative.Required {
		authType := strings.TrimSpace(alternative.AuthTypes[key])
		authName := strings.TrimSpace(alternative.AuthNames[key])
		// OAuth/OIDC without a connected-user selector is handled by the existing
		// connection-required path rather than suggesting a static provider token.
		if authType == "oauth" || authType == "oidc" {
			return nil
		}
		// Incomplete imported identity cannot authorize a credential mutation command.
		if authType == "" || authName == "" {
			continue
		}
		return &CredentialMaterialMissingError{
			BucketID: bucketID, ServiceID: serviceID, AuthType: authType, AuthName: authName,
		}
	}
	return nil
}

// shellQuoteCredentialArgument renders one bounded metadata value as a safe
// POSIX shell word without allowing substitutions or additional arguments.
func shellQuoteCredentialArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
