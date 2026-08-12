package sandbox

import (
	"errors"
	"net/url"
	"strings"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

var supportedOAuth2Flows = map[string]struct{}{
	"implicit": {}, "password": {}, "clientCredentials": {}, "authorizationCode": {},
}

// validateAuthRuntimeContract accepts only strategies the Engine can execute;
// preserving unknown auth metadata must not imply executable support.
func validateAuthRuntimeContract(auth fusedobject.AuthConfig) error {
	if auth.OAuth2MetadataURL != "" && !validOAuthMetadataEndpoint(auth.OAuth2MetadataURL) {
		return errors.New("runtime OAuth2 metadata URL is invalid")
	}
	if err := validateOAuth2Flows(auth.OAuth2Flows); err != nil {
		return err
	}
	if err := validateAuthPolicyProvenance(auth.PolicyProvenance); err != nil {
		return err
	}
	if auth.Strategy != nil {
		return validateAuthStrategy(auth)
	}
	canonical := authrouting.CanonicalType(auth.Type, auth.Scheme)
	if strings.EqualFold(auth.Type, "http") && canonical != "basic" && canonical != "bearer" {
		return unsupportedExecutionCapability()
	}
	if canonical == "oauth1" {
		return errors.New("runtime OAuth1 auth strategy is missing")
	}
	return nil
}

func validOAuthMetadataEndpoint(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.IsAbs() && parsed.User == nil && parsed.Scheme == "https" && parsed.Host != ""
}

func validateOAuth2Flows(flows map[string]fusedobject.OAuth2FlowContract) error {
	if len(flows) > 4 {
		return errors.New("runtime OAuth2 flow set is invalid")
	}
	for name, flow := range flows {
		if _, ok := supportedOAuth2Flows[name]; !ok || flow.Scopes == nil || len(flow.Scopes) > 256 {
			return errors.New("runtime OAuth2 flow is invalid")
		}
		if err := validateOAuth2FlowURLs(name, flow); err != nil {
			return err
		}
	}
	return nil
}

func validateOAuth2FlowURLs(name string, flow fusedobject.OAuth2FlowContract) error {
	if (name == "implicit" || name == "authorizationCode") && !validOAuthEndpoint(flow.AuthorizationURL) {
		return errors.New("runtime OAuth2 authorization URL is invalid")
	}
	if name != "implicit" && !validOAuthEndpoint(flow.TokenURL) {
		return errors.New("runtime OAuth2 token URL is invalid")
	}
	if flow.RefreshURL != "" && !validOAuthEndpoint(flow.RefreshURL) {
		return errors.New("runtime OAuth2 refresh URL is invalid")
	}
	return nil
}

// validOAuthEndpoint permits loopback HTTP for local development while keeping
// remotely hosted token and authorization exchanges on HTTPS.
func validOAuthEndpoint(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.IsAbs() && parsed.User == nil && (parsed.Scheme == "https" || (parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1")))
}

func validateAuthPolicyProvenance(values map[string]string) error {
	if len(values) > 64 {
		return errors.New("runtime auth policy provenance is too large")
	}
	for field, provenance := range values {
		if strings.TrimSpace(field) == "" || len(field) > 128 || !validPolicyProvenance(provenance) {
			return errors.New("runtime auth policy provenance is invalid")
		}
	}
	return nil
}

func validPolicyProvenance(value string) bool {
	return value == "source_spec" || value == "x-fused" || value == "reviewed_overlay"
}

func validateAuthStrategy(auth fusedobject.AuthConfig) error {
	switch auth.Strategy.Kind {
	case "oauth1_signature":
		return validateOAuth1Strategy(auth)
	case "http_challenge":
		return validateChallengeStrategy(auth)
	default:
		return errors.New("runtime auth strategy is unsupported")
	}
}

func validateOAuth1Strategy(auth fusedobject.AuthConfig) error {
	strategy := auth.Strategy.OAuth1
	if authrouting.CanonicalType(auth.Type, auth.Scheme) != "oauth1" || strategy == nil || auth.Strategy.Challenge != nil {
		return errors.New("runtime OAuth1 auth strategy is invalid")
	}
	if strategy.SignatureMethod != "hmac_sha1" && strategy.SignatureMethod != "hmac_sha256" {
		return errors.New("runtime OAuth1 signature method is unsupported")
	}
	if strategy.ParameterLocation != "authorization_header" {
		return errors.New("runtime OAuth1 parameter location is unsupported")
	}
	return nil
}

func validateChallengeStrategy(auth fusedobject.AuthConfig) error {
	strategy := auth.Strategy.Challenge
	if !strings.EqualFold(auth.Type, "http") || strategy == nil || auth.Strategy.OAuth1 != nil {
		return errors.New("runtime HTTP challenge strategy is invalid")
	}
	if strategy.Scheme != "digest" || !strings.EqualFold(auth.Scheme, "digest") {
		return unsupportedExecutionCapability()
	}
	return nil
}

func unsupportedExecutionCapability() error {
	return &fusedobject.ExecutionContractCompatibilityError{
		Reason:          fusedobject.ExecutionContractReasonUnsupportedCapability,
		ContractVersion: fusedobject.CurrentExecutionContractVersion,
		CapabilityCount: 1,
	}
}

func validateSecurityServerSelections(requirements authrouting.Requirements, servers fusedobject.Servers) error {
	declared := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		declared[server.URL] = struct{}{}
	}
	for _, alternative := range requirements {
		if err := validateSecurityServerSelection(alternative, declared); err != nil {
			return err
		}
	}
	return nil
}

func validateSecurityServerSelection(alternative authrouting.Alternative, servers map[string]struct{}) error {
	selection := alternative.ServerSelection
	if selection == nil {
		return nil
	}
	if len(alternative.Schemes) == 0 || strings.TrimSpace(selection.ServerURL) == "" {
		return errors.New("runtime security server selection is invalid")
	}
	if !alternativeContainsScheme(alternative, selection.Scheme) {
		return errors.New("runtime security server selection references unknown scheme")
	}
	if _, ok := servers[selection.ServerURL]; !ok {
		return errors.New("runtime security server selection references unknown server")
	}
	return nil
}

func alternativeContainsScheme(alternative authrouting.Alternative, wanted string) bool {
	for _, scheme := range alternative.Schemes {
		if scheme.Scheme == wanted {
			return true
		}
	}
	return false
}
