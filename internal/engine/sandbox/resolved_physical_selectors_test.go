package sandbox

import (
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestResolvedPhysicalOperationValidateSelectorsAcceptsExactContract protects the rule that caller routing selectors cannot exceed the compiled endpoint contract.
func TestResolvedPhysicalOperationValidateSelectorsAcceptsExactContract(t *testing.T) {
	operation := physicalSelectorTestOperation(true)
	err := operation.ValidateSelectors(PhysicalExecutionSelectors{
		Environment: "sandbox", EndUserRef: "user-1", AuthType: "oauth",
		AuthName: "oauthAuth", ResourceID: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("ValidateSelectors() error = %v", err)
	}
}

// TestResolvedPhysicalOperationValidateSelectorsRejectsContractMismatches protects the rule that caller routing selectors cannot exceed the compiled endpoint contract.
func TestResolvedPhysicalOperationValidateSelectorsRejectsContractMismatches(t *testing.T) {
	tests := []struct {
		name      string
		operation ResolvedPhysicalOperation
		selectors PhysicalExecutionSelectors
	}{
		{name: "environment", operation: physicalSelectorTestOperation(true), selectors: PhysicalExecutionSelectors{Environment: "staging"}},
		{name: "auth type", operation: physicalSelectorTestOperation(true), selectors: PhysicalExecutionSelectors{AuthType: "basic"}},
		{name: "auth name", operation: physicalSelectorTestOperation(true), selectors: PhysicalExecutionSelectors{AuthName: "other"}},
		{name: "auth excluded by endpoint", operation: physicalEndpointRestrictedSelectorTestOperation(), selectors: PhysicalExecutionSelectors{AuthType: "api_key", AuthName: "apiKeyAuth"}},
		{name: "resource without end user", operation: physicalSelectorTestOperation(true), selectors: PhysicalExecutionSelectors{AuthType: "oauth", ResourceID: uuid.NewString()}},
		{name: "resource contract missing", operation: physicalSelectorTestOperation(false), selectors: PhysicalExecutionSelectors{EndUserRef: "user-1", AuthType: "oauth", AuthName: "oauthAuth", ResourceID: uuid.NewString()}},
		{name: "invalid resource id", operation: physicalSelectorTestOperation(true), selectors: PhysicalExecutionSelectors{EndUserRef: "user-1", AuthType: "oauth", AuthName: "oauthAuth", ResourceID: "provider-id"}},
		{name: "connected auth unavailable", operation: physicalStaticSelectorTestOperation(), selectors: PhysicalExecutionSelectors{EndUserRef: "user-1"}},
		{name: "profile auth type mismatch", operation: physicalMismatchedProfileSelectorTestOperation("oidc", "oauthAuth"), selectors: PhysicalExecutionSelectors{EndUserRef: "user-1", AuthType: "oauth", AuthName: "oauthAuth"}},
		{name: "profile auth name mismatch", operation: physicalMismatchedProfileSelectorTestOperation("oauth", "other"), selectors: PhysicalExecutionSelectors{EndUserRef: "user-1", AuthType: "oauth", AuthName: "oauthAuth"}},
		{name: "planned static auth", operation: physicalPlannedStaticSelectorTestOperation(), selectors: PhysicalExecutionSelectors{EndUserRef: "user-1", ResourceID: uuid.NewString()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.operation.ValidateSelectors(test.selectors); !errors.Is(err, ErrPhysicalSelectorContract) {
				t.Fatalf("ValidateSelectors() error = %v, want ErrPhysicalSelectorContract", err)
			}
		})
	}
}

// physicalSelectorTestOperation builds an OAuth endpoint with optional resource
// discovery and both production and sandbox environments.
func physicalSelectorTestOperation(withResource bool) ResolvedPhysicalOperation {
	appID := uuid.New()
	serviceID, versionID, endpointID := uuid.New(), uuid.New(), uuid.New()
	profile := &fusedobject.ServiceConnectConfig{AuthType: "oauth", AuthName: "oauthAuth"}
	if withResource {
		profile.ResourceDiscovery = &fusedobject.ResourceDiscoveryConfig{}
	}
	service := &fusedobject.ServiceMetadata{
		ID: serviceID, ServiceVersionID: versionID, BaseURL: "https://api.example.test",
		Servers:     fusedobject.Servers{{URL: "https://api.example.test", Name: "production", IsDefault: true}, {URL: "https://sandbox.example.test", Name: "sandbox"}},
		AuthConfigs: fusedobject.AuthConfigs{{Name: "oauthAuth", Type: "oauth2"}}, ConnectConfig: profile,
	}
	endpoint := fusedobject.Endpoint{
		ID: endpointID, Name: "items.get",
		SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "oauthAuth"}}}},
	}
	selection := models.SDKSelection{ServiceID: serviceID, ServiceVersionID: versionID, EndpointIDs: []uuid.UUID{endpointID}}
	return ResolvedPhysicalOperation{appID: appID, match: &scopedEndpoint{
		service: service, endpoint: endpoint, allowed: true, serviceVersionID: versionID.String(), selection: selection,
	}}
}

// physicalStaticSelectorTestOperation replaces connected auth with basic auth
// to prove end-user selectors are rejected for static credentials.
func physicalStaticSelectorTestOperation() ResolvedPhysicalOperation {
	operation := physicalSelectorTestOperation(false)
	operation.match.service.ConnectConfig = nil
	operation.match.service.AuthConfigs = fusedobject.AuthConfigs{{Name: "basicAuth", Type: "http", Scheme: "basic"}}
	operation.match.endpoint.SecurityRequirements = authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "basicAuth"}}}}
	return operation
}

// physicalEndpointRestrictedSelectorTestOperation advertises an extra API-key
// scheme that the endpoint did not admit, isolating endpoint-level auth checks.
func physicalEndpointRestrictedSelectorTestOperation() ResolvedPhysicalOperation {
	operation := physicalSelectorTestOperation(false)
	operation.match.service.AuthConfigs = append(operation.match.service.AuthConfigs,
		fusedobject.AuthConfig{Name: "apiKeyAuth", Type: "apiKey"},
	)
	return operation
}

// physicalMismatchedProfileSelectorTestOperation changes the connect profile so
// requested auth type/name mismatches fail before physical accounting.
func physicalMismatchedProfileSelectorTestOperation(authType, authName string) ResolvedPhysicalOperation {
	operation := physicalSelectorTestOperation(true)
	operation.match.service.ConnectConfig.AuthType = authType
	operation.match.service.ConnectConfig.AuthName = authName
	return operation
}

// physicalPlannedStaticSelectorTestOperation models a plan pinned to API-key
// auth even though the endpoint can also satisfy OAuth.
func physicalPlannedStaticSelectorTestOperation() ResolvedPhysicalOperation {
	operation := physicalSelectorTestOperation(true)
	operation.match.service.AuthConfigs = append(operation.match.service.AuthConfigs,
		fusedobject.AuthConfig{Name: "apiKeyAuth", Type: "apiKey"},
	)
	operation.match.endpoint.SecurityRequirements = authrouting.Requirements{
		{Schemes: []authrouting.Requirement{{Scheme: "apiKeyAuth"}}},
		{Schemes: []authrouting.Requirement{{Scheme: "oauthAuth"}}},
	}
	operation.match.selection.AuthType = "api_key"
	operation.match.selection.AuthName = "apiKeyAuth"
	return operation
}
