package engine

import (
	"errors"
	"net/url"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/serverrouting"
)

// applySelectedSecurityServer permits auth-driven routing only when the exact
// selected alternative points at a server already reviewed in the contract.
func applySelectedSecurityServer(service *models.Service, operation *models.IntegrationObject, selected models.AuthConfigs, values []store.BucketValue) error {
	alternative, ok := selectedSecurityAlternative(operation.SecurityRequirements, selected)
	if !ok || alternative.ServerSelection == nil {
		return nil
	}
	server, ok := declaredSecurityServer(service, operation, alternative.ServerSelection.ServerURL)
	if !ok {
		return authRoutingError("invalid_contract")
	}
	supplied := securityServerVariables(service.ServerVariables, values)
	reference, _, err := serverrouting.ResolveReference(server.URL, server.Variables, supplied)
	if err != nil {
		return err
	}
	base := service.ServiceBaseURL
	if base == "" {
		base = service.BaseURL
	}
	resolved, err := resolveSecurityServerReference(base, reference)
	if err != nil {
		return err
	}
	service.BaseURL, service.ServerSource = resolved, "operation"
	return nil
}

func selectedSecurityAlternative(requirements authrouting.Requirements, selected models.AuthConfigs) (authrouting.Alternative, bool) {
	for _, alternative := range requirements {
		if sameAuthSchemeNames(alternative.Schemes, selected) {
			return alternative, true
		}
	}
	return authrouting.Alternative{}, false
}

func sameAuthSchemeNames(required []authrouting.Requirement, selected models.AuthConfigs) bool {
	if len(required) != len(selected) {
		return false
	}
	for index := range required {
		if required[index].Scheme != selected[index].Name {
			return false
		}
	}
	return true
}

// declaredSecurityServer checks operation scope first because an operation-level
// server intentionally overrides the same service-level routing decision.
func declaredSecurityServer(service *models.Service, operation *models.IntegrationObject, wanted string) (models.Server, bool) {
	for _, server := range operation.OperationServers {
		if server.URL == wanted {
			return server, true
		}
	}
	for _, server := range service.Servers {
		if server.URL == wanted {
			return server, true
		}
	}
	return models.Server{}, false
}

func securityServerVariables(configured map[string]string, values []store.BucketValue) map[string]string {
	result := make(map[string]string, len(configured)+len(values))
	for name, value := range configured {
		result[name] = value
	}
	for _, value := range values {
		if value.SourceKind == "connection_resource" && value.KeyName != "" {
			result[value.KeyName] = value.Value
		}
	}
	return result
}

func resolveSecurityServerReference(baseRaw, referenceRaw string) (string, error) {
	base, baseErr := url.Parse(baseRaw)
	reference, referenceErr := url.Parse(referenceRaw)
	if baseErr != nil || referenceErr != nil || !base.IsAbs() {
		return "", errors.New("security-selected server URL is invalid")
	}
	resolved := base.ResolveReference(reference).String()
	if err := serverrouting.ValidateResolvedURL(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}
