package sandbox

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

// validateInboundSecurity resolves documentary references without consulting or
// modifying outbound credentials, connection profiles, or signature policies.
func validateInboundSecurity(contract fusedobject.InboundOperationContract) error {
	// Match the canonical namespace budget rather than accepting unbounded passive metadata.
	if len(contract.SecuritySchemes) > 32 {
		return errors.New("runtime inbound security scheme set is too large")
	}
	definitions := make(map[string]string, len(contract.SecuritySchemes))
	for name, scheme := range contract.SecuritySchemes {
		// Exact identifiers must not alias after trimming or contain control bytes.
		if !validInboundSecurityText(name, 128) || name != strings.TrimSpace(name) {
			return errors.New("runtime inbound security scheme name is invalid")
		}
		// A matching name alone cannot legitimize an invalid or unresolved definition.
		if err := validateInboundSecurityScheme(scheme); err != nil {
			return err
		}
		definitions[name] = inboundSecurityScopeType(scheme.Type)
	}
	// Reuse endpoint OR-of-AND, duplicate, scope-type, and missing-name checks locally.
	if err := validateSecurityRequirements(contract.SecurityRequirements, definitions); err != nil {
		return err
	}
	for _, alternative := range contract.SecurityRequirements {
		for _, requirement := range alternative.Schemes {
			// OAuth scopes must additionally exist in the source's declared flow catalogue.
			if err := validateInboundDeclaredScopes(requirement, contract.SecuritySchemes[requirement.Scheme]); err != nil {
				return err
			}
		}
	}
	return nil
}

// inboundSecurityScopeType maps only OpenAPI's actual scope-bearing types;
// an arbitrary HTTP scheme named "oauth" must not inherit OAuth semantics.
func inboundSecurityScopeType(schemeType string) string {
	// Documentary HTTP names cannot select executable or scope-bearing credential families.
	switch schemeType {
	case "oauth2":
		return "oauth"
	case "openIdConnect":
		return "oidc"
	default:
		return "inbound"
	}
}

// validateInboundSecurityScheme checks source shape without requiring an
// executable auth strategy for documentary HTTP schemes or OAuth flows.
func validateInboundSecurityScheme(scheme fusedobject.InboundSecurityScheme) error {
	// Only standard OpenAPI scheme types can identify a persisted source definition.
	switch scheme.Type {
	case "apiKey":
		// Preserve the provider's exact API-key transport rather than guessing a header.
		if (scheme.In != "header" && scheme.In != "query" && scheme.In != "cookie") || !validInboundSecurityText(scheme.Name, 256) {
			return errors.New("runtime inbound apiKey location or name is invalid")
		}
	case "http":
		// Unknown-but-named HTTP schemes remain documentary, never executable strategies.
		if !validInboundSecurityText(scheme.Scheme, 64) {
			return errors.New("runtime inbound HTTP security scheme is invalid")
		}
	case "oauth2", "openIdConnect", "mutualTLS":
		// OAuth flow and scope checks below do not infer receiver verification support.
	default:
		return errors.New("runtime inbound security scheme type is invalid")
	}
	// OAuth-only metadata cannot silently decorate another security family.
	if scheme.Type != "oauth2" && (len(scheme.Flows) > 0 || scheme.OAuth2MetadataURL != "") {
		return errors.New("runtime inbound OAuth metadata requires OAuth2")
	}
	// Metadata URLs follow the same canonical HTTPS rule without being fetched here.
	if scheme.OAuth2MetadataURL != "" && !validOAuthMetadataEndpoint(strings.TrimSpace(scheme.OAuth2MetadataURL)) {
		return errors.New("runtime inbound OAuth metadata URL is invalid")
	}
	// Five standard flows include documentary device authorization, not Engine execution support.
	if len(scheme.Flows) > 5 {
		return errors.New("runtime inbound OAuth flow set is invalid")
	}
	for name, flow := range scheme.Flows {
		// Every flow retains a checked scope catalogue before requirements may reference it.
		if err := validateInboundOAuthFlow(name, flow); err != nil {
			return err
		}
	}
	return nil
}

// validateInboundOAuthFlow follows canonical documentary flow requirements;
// it neither retrieves metadata nor acquires tokens from the declared URLs.
func validateInboundOAuthFlow(name string, flow fusedobject.InboundOAuthFlow) error {
	// Standard names are a finite vocabulary even though these flows are passive.
	switch name {
	case "implicit", "password", "clientCredentials", "authorizationCode", "deviceAuthorization":
		// These source flow names have known mandatory endpoint fields.
	default:
		return errors.New("runtime inbound OAuth flow name is invalid")
	}
	// Scope declarations must remain present even when empty; only used scopes share runtime requirement limits.
	if flow.Scopes == nil {
		return errors.New("runtime inbound OAuth flow scopes are invalid")
	}
	// Browser flows require the source authorization endpoint, not a fabricated default.
	if (name == "implicit" || name == "authorizationCode") && strings.TrimSpace(flow.AuthorizationURL) == "" {
		return errors.New("runtime inbound OAuth authorization URL is missing")
	}
	// Implicit is the sole standard flow without a token endpoint requirement.
	if name != "implicit" && strings.TrimSpace(flow.TokenURL) == "" {
		return errors.New("runtime inbound OAuth token URL is missing")
	}
	// Device authorization has a canonical HTTPS endpoint requirement despite being documentary.
	if name == "deviceAuthorization" && !validOAuthMetadataEndpoint(strings.TrimSpace(flow.DeviceAuthorizationURL)) {
		return errors.New("runtime inbound device authorization URL is invalid")
	}
	return nil
}

// validateInboundDeclaredScopes prevents OAuth requirements from referencing
// undeclared scopes while leaving OIDC's externally discovered scopes documentary.
func validateInboundDeclaredScopes(requirement authrouting.Requirement, scheme fusedobject.InboundSecurityScheme) error {
	// Other types already passed the shared type-aware requirement validator.
	if scheme.Type != "oauth2" {
		return nil
	}
	declared := make(map[string]struct{})
	for _, flow := range scheme.Flows {
		for scope := range flow.Scopes {
			declared[scope] = struct{}{}
		}
	}
	for _, scope := range requirement.Scopes {
		// A well-formed OAuth scope is still invalid when the source never declares it.
		if _, exists := declared[scope]; !exists {
			return errors.New("runtime inbound OAuth requirement references undeclared scope")
		}
	}
	return nil
}

// validInboundSecurityText keeps documentary identifiers exact, bounded, and safe for storage.
func validInboundSecurityText(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && len(value) <= limit && !strings.ContainsAny(value, "\x00\r\n")
}
