package sandbox

import (
	"errors"
	"regexp"
	"strings"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/catalogcontract"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/serverrouting"
	"github.com/Usefused/engine/internal/shared/signaturepolicy"
)

var serverPlaceholderPattern = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_.-]*)\}`)

// OpenAPI permits an unbounded OR list. Keep a generous runtime bound so the
// Engine accepts canonical generated contracts without accepting unbounded data.
const maxRuntimeSecurityAlternatives = 256

// validateTransportContract is the single snapshot-admission boundary so every
// operation is rejected before persistence when any executable surface drifts.
func validateTransportContract(metadata *fusedobject.ServiceMetadata, endpoints []fusedobject.Endpoint, webhooks []fusedobject.Webhook) error {
	if metadata == nil {
		return errors.New("runtime transport contract is missing")
	}
	if err := catalogcontract.Validate(metadata.Catalog); err != nil {
		return err
	}
	if err := validateIncomingWebhookContract(metadata); err != nil {
		return err
	}
	definitions, err := validateAuthDefinitions(metadata.AuthConfigs)
	if err != nil {
		return err
	}
	if err := validateRuntimeServers(metadata.Servers); err != nil {
		return err
	}
	if err := validateEndpointContracts(endpoints, definitions); err != nil {
		return err
	}
	return validatePassiveContracts(metadata, endpoints, webhooks, definitions)
}

func validateIncomingWebhookContract(metadata *fusedobject.ServiceMetadata) error {
	if metadata.IncomingWebhookConfig == nil {
		return nil
	}
	return signaturepolicy.Validate(metadata.IncomingWebhookConfig.SignaturePolicy)
}

func validateEndpointContracts(endpoints []fusedobject.Endpoint, definitions map[string]string) error {
	for _, endpoint := range endpoints {
		if err := validateEndpointTransport(endpoint); err != nil {
			return err
		}
		if err := validateSecurityRequirements(endpoint.SecurityRequirements, definitions); err != nil {
			return err
		}
		if err := validateSecurityServerSelections(endpoint.SecurityRequirements, endpoint.OperationServers); err != nil {
			return err
		}
	}
	return nil
}

func validateAuthDefinitions(auths fusedobject.AuthConfigs) (map[string]string, error) {
	seen := make(map[string]string, len(auths))
	for _, auth := range auths {
		if auth.Name == "" || len(auth.Name) > 128 {
			return nil, errors.New("runtime auth config name is invalid")
		}
		if _, exists := seen[auth.Name]; exists {
			return nil, errors.New("runtime auth config name is duplicated")
		}
		seen[auth.Name] = authrouting.CanonicalType(auth.Type, auth.Scheme)
		if err := validateBasicMode(auth); err != nil {
			return nil, err
		}
		if err := validateAuthRuntimeContract(auth); err != nil {
			return nil, err
		}
	}
	return seen, nil
}

func validateBasicMode(auth fusedobject.AuthConfig) error {
	isBasic := authrouting.CanonicalType(auth.Type, auth.Scheme) == "basic"
	if !isBasic && auth.BasicPasswordMode != "" {
		return errors.New("basic password mode is invalid for auth scheme")
	}
	if !isBasic {
		return nil
	}
	switch auth.BasicPasswordMode {
	case authrouting.BasicPasswordRequired, authrouting.BasicPasswordOptional, authrouting.BasicPasswordEmpty:
		return nil
	default:
		return errors.New("basic password mode is missing or invalid")
	}
}

func validateSecurityRequirements(requirements authrouting.Requirements, definitions map[string]string) error {
	if requirements == nil || len(requirements) == 0 || len(requirements) > maxRuntimeSecurityAlternatives {
		return errors.New("runtime security requirements are invalid")
	}
	for _, alternative := range requirements {
		if err := validateSecurityAlternative(alternative, definitions); err != nil {
			return err
		}
	}
	return nil
}

func validateSecurityAlternative(alternative authrouting.Alternative, definitions map[string]string) error {
	if len(alternative.Schemes) > 16 {
		return errors.New("runtime security alternative is too large")
	}
	seen := make(map[string]struct{}, len(alternative.Schemes))
	for _, requirement := range alternative.Schemes {
		authType, ok := definitions[requirement.Scheme]
		if !ok {
			return errors.New("runtime security requirement references unknown scheme")
		}
		if _, duplicate := seen[requirement.Scheme]; duplicate {
			return errors.New("runtime security requirement duplicates a scheme")
		}
		seen[requirement.Scheme] = struct{}{}
		if len(requirement.Scopes) > 64 {
			return errors.New("runtime security requirement has too many scopes")
		}
		if err := validateRequirementScopes(authType, requirement.Scopes); err != nil {
			return err
		}
	}
	return nil
}

func validateRequirementScopes(authType string, scopes []string) error {
	if len(scopes) > 0 && authType != "oauth" && authType != "oidc" {
		return errors.New("runtime security scopes are invalid for auth scheme")
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if strings.TrimSpace(scope) == "" || len(scope) > 256 {
			return errors.New("runtime security scope is invalid")
		}
		if _, exists := seen[scope]; exists {
			return errors.New("runtime security scope is duplicated")
		}
		seen[scope] = struct{}{}
	}
	return nil
}

func validateRuntimeServers(servers fusedobject.Servers) error {
	if err := validateRuntimeServerNames(servers); err != nil {
		return err
	}
	for _, server := range servers {
		if err := validateRuntimeServer(server); err != nil {
			return err
		}
	}
	return nil
}

func validateRuntimeServer(server fusedobject.Server) error {
	placeholders := serverPlaceholders(server.URL)
	variables := make(map[string]struct{}, len(server.Variables))
	for _, variable := range server.Variables {
		if err := addRuntimeServerVariable(variables, variable); err != nil {
			return err
		}
	}
	if !sameStringSet(placeholders, variables) {
		return errors.New("runtime server variables do not match template")
	}
	return nil
}

func addRuntimeServerVariable(variables map[string]struct{}, variable serverrouting.Variable) error {
	if variable.Name == "" || len(variable.Name) > 128 {
		return errors.New("runtime server variable name is invalid")
	}
	if _, exists := variables[variable.Name]; exists {
		return errors.New("runtime server variable is duplicated")
	}
	variables[variable.Name] = struct{}{}
	if variable.Default != nil && len(variable.Enum) > 0 && !stringInList(variable.Enum, *variable.Default) {
		return errors.New("runtime server variable default is outside enum")
	}
	return nil
}

// validateRuntimeServerNames compares canonical selectors case-insensitively;
// two SDK choices must never resolve to different servers by casing alone.
func validateRuntimeServerNames(servers fusedobject.Servers) error {
	seen := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		name := runtimeServerName(server.Name, server.Environment)
		if len(name) > 128 || strings.ContainsAny(name, "\r\n") {
			return errors.New("runtime server name is invalid")
		}
		key := strings.ToLower(name)
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("runtime server name is duplicated")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func serverPlaceholders(value string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, match := range serverPlaceholderPattern.FindAllStringSubmatch(value, -1) {
		result[match[1]] = struct{}{}
	}
	return result
}

func sameStringSet(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if _, ok := right[key]; !ok {
			return false
		}
	}
	return true
}

func stringInList(values []string, wanted string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == wanted {
			return true
		}
	}
	return false
}
