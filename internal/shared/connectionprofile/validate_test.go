package connectionprofile

import (
	"strings"
	"testing"
)

func TestValidateCanonicalProfile(t *testing.T) {
	profile := validProfile()
	contract := Contract{
		AuthTypes: []string{"oauth2"},
		Servers:   []string{"api"},
		Complete:  true,
		Operations: []Operation{
			{ID: "getAccessibleResources", Method: "GET"},
			{ID: "getAccount", Method: "GET", Parameters: []Parameter{{Name: "X-Account-ID", Location: "header"}}},
		},
	}
	result := Validate(&profile, contract)
	if result.HasErrors() {
		t.Fatalf("Validate errors = %#v", result.Issues)
	}
	if len(result.Warnings()) != 0 {
		t.Fatalf("Validate warnings = %#v", result.Warnings())
	}
}

// TestValidateNormalizesDiscoveryLifecycle keeps omitted optional fields
// deterministic across OpenAPI, Postman, workspace config, and GraphQL writes.
func TestValidateNormalizesDiscoveryLifecycle(t *testing.T) {
	profile := validProfile()
	result := Validate(&profile, Contract{})
	if result.HasErrors() {
		t.Fatalf("Validate errors = %#v", result.Issues)
	}
	if profile.ResourceDiscovery.AutoRun != "after_oauth_callback" || profile.ResourceDiscovery.Lifecycle != "authoritative" {
		t.Fatalf("discovery defaults = %#v", profile.ResourceDiscovery)
	}
}

// TestValidateRejectsUnsupportedDiscoveryLifecycle prevents metadata values
// that runtime would otherwise silently ignore.
func TestValidateRejectsUnsupportedDiscoveryLifecycle(t *testing.T) {
	profile := validProfile()
	profile.ResourceDiscovery.AutoRun = "manual"
	profile.ResourceDiscovery.Lifecycle = "append"
	result := Validate(&profile, Contract{})
	assertIssueCodes(t, result, "discovery.auto_run.invalid", "discovery.lifecycle.invalid")
}

// TestValidateStaticHostResourceContext permits provider resource IDs used by
// request bindings without requiring dynamic host configuration.
func TestValidateStaticHostResourceContext(t *testing.T) {
	profile := Profile{
		AuthType: "oauth",
		ResourceDiscovery: &ResourceDiscoveryConfig{
			OperationID: "listPortals", IDPath: "$[*].id", ResourceType: "portal",
		},
		Bindings: []Binding{{
			Value: "${resource.provider_resource_id}", Location: "query", Name: "portalId", Mode: "force", ProviderExtension: true,
		}},
	}
	result := Validate(&profile, Contract{})
	if result.HasErrors() {
		t.Fatalf("Validate errors = %#v", result.Issues)
	}
}

func TestValidateRejectsUnsafeAndAmbiguousBindings(t *testing.T) {
	profile := validProfile()
	profile.Bindings = []Binding{
		{Value: "${resource.metadata.undeclared}", Location: "header", Name: "Authorization", Mode: "force", Operations: []string{"getAccount"}},
		{Value: "${resource.base_url}", Location: "body", Name: "tenant", Mode: "force", Operations: []string{"getAccount"}},
		{Value: "${resource.base_url}", Location: "base_url", Mode: "default"},
	}
	contract := Contract{Operations: []Operation{{ID: "getAccessibleResources", Method: "GET"}, {ID: "getAccount", Method: "GET"}}}
	result := Validate(&profile, contract)
	assertIssueCodes(t, result, "binding.header.protected", "binding.metadata.unknown", "binding.dynamic_body.unsupported", "binding.base_url.invalid")
}

func TestValidateContractTargets(t *testing.T) {
	profile := validProfile()
	profile.Bindings = []Binding{
		{Value: "tenant", Location: "path", Name: "tenantId", Mode: "force", Operations: []string{"getAccount"}},
		{Value: "tenant", Location: "query", Name: "portalId", Mode: "default", Operations: []string{"getAccount"}},
	}
	contract := Contract{Operations: []Operation{
		{ID: "getAccessibleResources", Method: "GET"},
		{ID: "getAccount", Method: "GET", Parameters: []Parameter{{Name: "accountId", Location: "path"}}},
	}}
	result := Validate(&profile, contract)
	assertIssueCodes(t, result, "binding.path.unknown", "binding.target.extension")
}

func TestValidateInputTemplateAndPatterns(t *testing.T) {
	profile := Profile{AuthType: "oauth", ResourceInput: &ResourceInputConfig{
		Fields: []ResourceInputField{
			{Name: "subdomain", Pattern: "["},
			{Name: "subdomain"},
		},
		BaseURLTemplate: "https://{undeclared}.zendesk.com/api/v2",
		ResourceType:    "zendesk_subdomain",
	}}
	result := Validate(&profile, Contract{})
	assertIssueCodes(t, result, "resource_input.pattern.invalid", "resource_input.name.duplicate", "resource_input.template.unknown_field", "resource_input.allowed_hosts.required")
}

func TestValidationErrorsDoNotLeakSensitiveConfig(t *testing.T) {
	secretURL := "https://customer-secret.invalid/{tenant}"
	profile := Profile{AuthType: "oauth", ResourceInput: &ResourceInputConfig{
		Fields:          []ResourceInputField{{Name: "subdomain"}},
		BaseURLTemplate: secretURL,
		ResourceType:    "tenant",
	}}
	err := Validate(&profile, Contract{}).Err()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if strings.Contains(err.Error(), secretURL) || strings.Contains(err.Error(), "customer-secret") {
		t.Fatalf("validation error leaked config: %v", err)
	}
}

// TestValidateLiteralBucketValueRejectsPrivilegedTargets protects every API
// edge that still accepts the legacy static bucket-value shape.
func TestValidateLiteralBucketValueRejectsPrivilegedTargets(t *testing.T) {
	for _, input := range []struct{ location, name, value string }{
		{"base_url", "", "https://attacker.example"},
		{"header", "Authorization", "Bearer unsafe"},
		{"header", "X-Tenant", "safe\r\nInjected: true"},
	} {
		if err := ValidateLiteralBucketValue(input.location, input.name, input.value); err == nil {
			t.Fatalf("accepted literal bucket value: %#v", input)
		}
	}
}

func validProfile() Profile {
	return Profile{
		AuthType: "oauth",
		ResourceDiscovery: &ResourceDiscoveryConfig{
			OperationID:  "getAccessibleResources",
			Server:       "api",
			IDPath:       "$[*].id",
			NamePath:     "$[*].name",
			BaseURLPath:  "$[*].url",
			ResourceType: "jira_site",
			AllowedHosts: []string{
				"api.atlassian.com",
			},
		},
		Metadata: map[string]string{"account_id": "$[*].accountId"},
		Bindings: []Binding{
			{Value: "${resource.base_url}", Location: "base_url", Mode: "force"},
			{Value: "${resource.metadata.account_id}", Location: "header", Name: "X-Account-ID", Mode: "force", Operations: []string{"getAccount"}},
		},
	}
}

func assertIssueCodes(t *testing.T, result Result, expected ...string) {
	t.Helper()
	codes := map[string]bool{}
	for _, issue := range result.Issues {
		codes[issue.Code] = true
	}
	for _, code := range expected {
		if !codes[code] {
			t.Errorf("missing issue %q in %#v", code, result.Issues)
		}
	}
}
