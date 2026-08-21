package connectionprofile

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestProfileJSONRejectsUnknownFieldsRecursively proves Engine profile decoding stays closed at every nested boundary.
func TestProfileJSONRejectsUnknownFieldsRecursively(t *testing.T) {
	payloads := []string{
		`{"auth_type":"oauth","resource_discovery":{"version":1,"stage":"post_auth","operation_id":"list","id_path":"$.id","resource_type":"tenant","unknown":true}}`,
		`{"auth_type":"oauth","resource_input":{"fields":[{"name":"tenant","unknown":true}],"base_url_template":"https://{tenant}.example.com","resource_type":"tenant"}}`,
		`{"auth_type":"oauth"} {"auth_type":"oauth"}`,
	}
	// Each payload targets a different nested or trailing-data bypass of ordinary JSON decoding.
	for _, payload := range payloads {
		var profile Profile
		// Unknown nested fields and second values must never survive into validation or persistence.
		if err := json.Unmarshal([]byte(payload), &profile); err == nil {
			t.Fatalf("expected strict decode failure for %s", payload)
		}
	}
}

// TestValidateDiscoveryInputMatch permits customer input only as an exact
// constraint on provider-discovered metadata and canonicalizes its key.
func TestValidateDiscoveryInputMatch(t *testing.T) {
	profile := validProfile()
	profile.Metadata["site_url"] = "$[*].url"
	profile.ResourceInput = validMatchedInput(" site_url ")
	result := Validate(&profile, Contract{})
	// The explicit equality constraint is the only safe composition of both resource sources.
	if result.HasErrors() {
		t.Fatalf("Validate errors = %#v", result.Issues)
	}
	// Validation normalizes the persisted lookup key before hashing or runtime use.
	if profile.ResourceInput.DiscoveryMatch.MetadataKey != "site_url" {
		t.Fatalf("normalized match key = %q", profile.ResourceInput.DiscoveryMatch.MetadataKey)
	}
}

// TestNormalizeDiscoveryInputMatchIsHashStable proves insignificant key
// whitespace cannot produce a different persisted profile representation.
func TestNormalizeDiscoveryInputMatchIsHashStable(t *testing.T) {
	spaced := validProfile()
	spaced.Metadata["site_url"] = "$[*].url"
	spaced.ResourceInput = validMatchedInput(" site_url ")
	canonical := validProfile()
	canonical.Metadata["site_url"] = "$[*].url"
	canonical.ResourceInput = validMatchedInput("site_url")
	Normalize(&spaced)
	Normalize(&canonical)
	spacedJSON, _ := json.Marshal(spaced)
	canonicalJSON, _ := json.Marshal(canonical)
	// Equivalent author input must produce byte-identical canonical JSON.
	if string(spacedJSON) != string(canonicalJSON) {
		t.Fatalf("normalized profiles differ: %s != %s", spacedJSON, canonicalJSON)
	}
}

// TestValidateRejectsUnsafeDiscoveryInputComposition covers every profile
// shape that would make customer selection decorative or ambiguous.
func TestValidateRejectsUnsafeDiscoveryInputComposition(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Profile)
		code string
	}{
		{"missing matcher", func(profile *Profile) {
			profile.ResourceInput = validMatchedInput("")
			profile.ResourceInput.DiscoveryMatch = nil
		}, "resource_source.conflict"},
		{"missing metadata", func(profile *Profile) { profile.ResourceInput = validMatchedInput("site_url") }, "resource_input.discovery_match.metadata_missing"},
		{"resource type mismatch", func(profile *Profile) {
			profile.Metadata["site_url"] = "$[*].url"
			profile.ResourceInput = validMatchedInput("site_url")
			profile.ResourceInput.ResourceType = "other"
		}, "resource_input.discovery_match.resource_type"},
		{"matcher without discovery", func(profile *Profile) {
			profile.ResourceDiscovery = nil
			profile.ResourceInput = validMatchedInput("site_url")
		}, "resource_input.discovery_match.unavailable"},
	}
	// Each invalid composition must surface its stable public issue code.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := validProfile()
			test.edit(&profile)
			assertIssueCodes(t, Validate(&profile, Contract{}), test.code)
		})
	}
}

// validMatchedInput returns the constrained Jira-style input used by
// composition tests without duplicating security-sensitive routing fields.
func validMatchedInput(metadataKey string) *ResourceInputConfig {
	return &ResourceInputConfig{
		Fields:          []ResourceInputField{{Name: "subdomain", Required: true, Pattern: `^[a-z0-9-]+$`}},
		BaseURLTemplate: "https://{subdomain}.atlassian.net", ResourceType: "jira_site",
		AllowedHosts: []string{"*.atlassian.net"}, DiscoveryMatch: &ResourceInputDiscoveryMatch{MetadataKey: metadataKey},
	}
}

func TestValidateCanonicalProfile(t *testing.T) {
	profile := validProfile()
	contract := Contract{
		AuthConfigs: []AuthConfig{{Name: "oauth", Type: "oauth2", OAuth2Flows: []string{"authorizationCode"}}},
		Servers:     []string{"api"},
		Complete:    true,
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

// TestNormalizeResourceInputFieldsCanonicalizesPresentation proves legacy text
// defaults and insignificant presentation whitespace share one stored profile.
func TestNormalizeResourceInputFieldsCanonicalizesPresentation(t *testing.T) {
	profile := resourceInputProfile([]ResourceInputField{
		{Name: "tenant", Label: " Tenant ", Placeholder: " Example ", Description: " Pick one ", Options: nil},
		{Name: "region", Type: " SELECT ", Options: []ResourceInputOption{{Value: " eu ", Label: " Europe "}}},
	})
	Normalize(&profile)
	textField, selectField := profile.ResourceInput.Fields[0], profile.ResourceInput.Fields[1]
	// Omitted types retain the existing text behavior after canonicalization.
	if textField.Type != ResourceInputFieldTypeText || textField.Label != "Tenant" || textField.Placeholder != "Example" || textField.Description != "Pick one" {
		t.Fatalf("normalized text field = %#v", textField)
	}
	// Select identity and option presentation values are canonical before hashing.
	if selectField.Type != ResourceInputFieldTypeSelect || selectField.Options[0] != (ResourceInputOption{Value: "eu", Label: "Europe"}) {
		t.Fatalf("normalized select field = %#v", selectField)
	}
}

// TestValidateResourceInputFieldTypes accepts the closed provider-neutral text
// and select grammar while preserving the legacy omitted-type form.
func TestValidateResourceInputFieldTypes(t *testing.T) {
	fields := []ResourceInputField{
		{Name: "tenant", Required: true, Pattern: `^[a-z]+$`},
		{Name: "region", Type: ResourceInputFieldTypeSelect, Options: []ResourceInputOption{{Value: "eu", Label: "Europe"}, {Value: "us"}}},
	}
	result := Validate(resourceInputProfilePointer(fields), Contract{})
	// Both field kinds remain string-valued and pass the same routing validation.
	if result.HasErrors() {
		t.Fatalf("Validate errors = %#v", result.Issues)
	}
}

// TestValidateResourceInputFieldConstraints covers every bounded type,
// presentation, pattern, and option failure with stable issue codes.
func TestValidateResourceInputFieldConstraints(t *testing.T) {
	tests := []struct {
		name   string
		fields []ResourceInputField
		codes  []string
	}{
		{name: "unknown type", fields: []ResourceInputField{{Name: "tenant", Type: "radio"}}, codes: []string{"resource_input.type.invalid"}},
		{name: "text options", fields: []ResourceInputField{{Name: "tenant", Options: []ResourceInputOption{{Value: "one"}}}}, codes: []string{"resource_input.options.unavailable"}},
		{name: "select options required", fields: []ResourceInputField{{Name: "tenant", Type: ResourceInputFieldTypeSelect}}, codes: []string{"resource_input.options.required"}},
		{name: "select pattern unavailable", fields: []ResourceInputField{{Name: "tenant", Type: ResourceInputFieldTypeSelect, Pattern: "tenant", Options: []ResourceInputOption{{Value: "one"}}}}, codes: []string{"resource_input.pattern.unavailable"}},
		{name: "presentation limits", fields: []ResourceInputField{{Name: "tenant", Label: strings.Repeat("界", 257), Placeholder: strings.Repeat("界", 257), Description: strings.Repeat("界", 513)}}, codes: []string{"resource_input.label.limit", "resource_input.placeholder.limit", "resource_input.description.limit"}},
		{name: "pattern limit", fields: []ResourceInputField{{Name: "tenant", Pattern: strings.Repeat("a", 1025)}}, codes: []string{"resource_input.pattern.limit"}},
		{name: "option values", fields: []ResourceInputField{{Name: "tenant", Type: ResourceInputFieldTypeSelect, Options: []ResourceInputOption{{Value: "", Label: strings.Repeat("界", 257)}, {Value: "same"}, {Value: "same"}, {Value: strings.Repeat("界", 257)}}}}, codes: []string{"resource_input.option.value.required", "resource_input.option.label.limit", "resource_input.option.value.duplicate", "resource_input.option.value.limit"}},
	}
	// Each malformed declaration must fail at profile validation, not hosted-form submission.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertIssueCodes(t, Validate(resourceInputProfilePointer(test.fields), Contract{}), test.codes...)
		})
	}
}

// TestValidateResourceInputCollectionLimits bounds both fields and select
// options before profile persistence or hosted-form projection.
func TestValidateResourceInputCollectionLimits(t *testing.T) {
	fields := make([]ResourceInputField, 33)
	for index := range fields {
		fields[index] = ResourceInputField{Name: fmt.Sprintf("field_%d", index)}
	}
	profile := resourceInputProfilePointer(fields)
	profile.ResourceInput.BaseURLTemplate = "https://{field_0}.example.com"
	assertIssueCodes(t, Validate(profile, Contract{}), "resource_input.fields.limit")
	options := make([]ResourceInputOption, 101)
	for index := range options {
		options[index] = ResourceInputOption{Value: fmt.Sprintf("option_%d", index)}
	}
	selectField := []ResourceInputField{{Name: "tenant", Type: ResourceInputFieldTypeSelect, Options: options}}
	assertIssueCodes(t, Validate(resourceInputProfilePointer(selectField), Contract{}), "resource_input.options.limit")
}

// resourceInputProfilePointer returns a complete input-only profile for focused
// field validation without introducing discovery composition errors.
func resourceInputProfilePointer(fields []ResourceInputField) *Profile {
	profile := resourceInputProfile(fields)
	return &profile
}

// resourceInputProfile returns a complete input-only profile whose routing
// template references the tenant field used by focused validation tests.
func resourceInputProfile(fields []ResourceInputField) Profile {
	return Profile{AuthType: "oauth", ResourceInput: &ResourceInputConfig{
		Fields: fields, BaseURLTemplate: "https://{tenant}.example.com", ResourceType: "tenant", AllowedHosts: []string{"*.example.com"},
	}}
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
