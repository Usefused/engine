package sandbox

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

// ErrPhysicalSelectorContract identifies a caller selector that cannot apply
// to the exact resolved provider operation. It deliberately carries no
// selector or contract values across the sandbox boundary.
var ErrPhysicalSelectorContract = errors.New("physical execution selector is incompatible with resolved operation")

// PhysicalExecutionSelectors is the non-secret routing surface that may be
// checked before a physical execution starts. Provider URLs, credentials, and
// resolved connection data never enter this DTO.
type PhysicalExecutionSelectors struct {
	Environment string
	EndUserRef  string
	AuthType    string
	AuthName    string
	ResourceID  string
}

// ValidateSelectors checks one caller selector against the immutable metadata
// already captured by exact resolution. It performs no store, provider, or
// secret lookup, so callers can validate the complete target set atomically
// before any child enters physical accounting.
func (operation ResolvedPhysicalOperation) ValidateSelectors(selectors PhysicalExecutionSelectors) error {
	match, err := operation.selectorContractMatch()
	if err != nil {
		return err
	}
	if err := validateResolvedEnvironmentSelector(match.service, selectors.Environment); err != nil {
		return err
	}
	credentials := physicalSelectorCredentials(selectors)
	// physical execution applies the SDK selection only when the caller did
	// not choose auth explicitly. Preflight must inspect that same effective
	// route or a planned static branch could be mistaken for connected auth.
	credentials = credentialsWithSelectionAuth(credentials, match.selection, match.endpoint.SecurityRequirements)
	if _, err := optionalConnectionResourceID(credentials); err != nil {
		return physicalSelectorContractError("resource")
	}
	definitions, err := fusedAuthDefinitions(match.service.AuthConfigs)
	if err != nil {
		return err
	}
	if !resolvedAuthSelectorsAllowed(match.endpoint.SecurityRequirements, definitions, credentials) {
		return physicalSelectorContractError("auth")
	}
	return validateResolvedConnectSelectors(match, selectors, credentials, definitions)
}

// selectorContractMatch finds the compiled service selection that owns a resolved physical operation.
func (operation ResolvedPhysicalOperation) selectorContractMatch() (*scopedEndpoint, error) {
	if operation.appID == uuid.Nil || operation.match == nil || operation.match.service == nil || !operation.match.allowed {
		return nil, errors.New("resolved physical operation is invalid")
	}
	return operation.match, nil
}

// validateResolvedEnvironmentSelector rejects malformed resolved environment selector before it can cross the physical selector preflight boundary.
func validateResolvedEnvironmentSelector(service *fusedobject.ServiceMetadata, environment string) error {
	if _, err := resolveRuntimeEnvironment(service, environment); err != nil {
		return physicalSelectorContractError("environment")
	}
	return nil
}

// physicalSelectorCredentials creates only reserved credential-routing keys accepted by the resolver.
func physicalSelectorCredentials(selectors PhysicalExecutionSelectors) map[string]any {
	credentials := make(map[string]any, 4)
	addPhysicalSelector(credentials, "fused_end_user_ref", selectors.EndUserRef)
	addPhysicalSelector(credentials, credentialKeyFusedAuthType, selectors.AuthType)
	addPhysicalSelector(credentials, credentialKeyFusedAuthName, selectors.AuthName)
	addPhysicalSelector(credentials, "fused_resource_id", selectors.ResourceID)
	return credentials
}

// addPhysicalSelector adds one nonempty reserved selector key without overwriting other routing state.
func addPhysicalSelector(credentials map[string]any, key, value string) {
	if value != "" {
		credentials[key] = value
	}
}

// resolvedAuthSelectorsAllowed checks auth type and profile against the immutable endpoint selection.
func resolvedAuthSelectorsAllowed(requirements authrouting.Requirements, definitions map[string]fusedobject.AuthConfig, credentials map[string]any) bool {
	for _, alternative := range requirements {
		if alternativeMatchesSelectors(alternative, definitions, credentials) {
			return true
		}
	}
	return false
}

// validateResolvedConnectSelectors rejects malformed resolved connect selectors before it can cross the physical selector preflight boundary.
func validateResolvedConnectSelectors(match *scopedEndpoint, selectors PhysicalExecutionSelectors, credentials map[string]any, definitions map[string]fusedobject.AuthConfig) error {
	required := connectedAuthResolutionRequired(selectors.EndUserRef, credentials, match.endpoint.SecurityRequirements)
	if !required {
		if selectors.ResourceID != "" {
			return physicalSelectorContractError("resource")
		}
		return nil
	}
	authName, err := selectedConnectedAuthName(credentials, match.service.AuthConfigs, match.endpoint.SecurityRequirements)
	if err != nil || authName == "" {
		return physicalSelectorContractError("end_user")
	}
	auth, ok := definitions[authName]
	if !ok || !isConnectedAuthSelector(canonicalFusedAuthType(auth)) {
		return physicalSelectorContractError("end_user")
	}
	if !resolvedConnectProfileAllows(match.service.ConnectConfig, auth) {
		return physicalSelectorContractError("connect")
	}
	if selectors.ResourceID != "" && !resolvedResourceSelectorAllowed(match.service.ConnectConfig) {
		return physicalSelectorContractError("resource")
	}
	return nil
}

// resolvedConnectProfileAllows matches a requested profile against the compiled connection-profile contract.
func resolvedConnectProfileAllows(profile *fusedobject.ServiceConnectConfig, auth fusedobject.AuthConfig) bool {
	if profile == nil {
		return true
	}
	selectedType := canonicalFusedAuthType(auth)
	profileType := connectionprofile.CanonicalAuthType(profile.AuthType)
	if profileType == "" || profileType != selectedType {
		return false
	}
	return strings.TrimSpace(profile.AuthName) == "" || strings.TrimSpace(profile.AuthName) == authCredentialName(auth)
}

// resolvedResourceSelectorAllowed permits resource routing only when the service contract declared it.
func resolvedResourceSelectorAllowed(profile *fusedobject.ServiceConnectConfig) bool {
	return profile != nil && (profile.ResourceDiscovery != nil || profile.ResourceInput != nil)
}

// physicalSelectorContractError returns a stable predispatch error without entering physical accounting.
func physicalSelectorContractError(kind string) error {
	return fmt.Errorf("%w: %s", ErrPhysicalSelectorContract, kind)
}
