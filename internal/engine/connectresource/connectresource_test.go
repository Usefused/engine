package connectresource

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/testcontract"
)

// TestNormalizeInputAcceptsPartialOptionalInput confirms callers can omit an
// optional field without being sent through the browser collection flow.
func TestNormalizeInputAcceptsPartialOptionalInput(t *testing.T) {
	config := normalizeInputTestConfig()
	normalized, missing, err := NormalizeInput(config, map[string]string{"subdomain": " acme "})
	// A declared required value is sufficient even when optional context is absent.
	if err != nil {
		t.Fatalf("NormalizeInput: %v", err)
	}
	// Missing is reserved for required fields that the collection page must request.
	if len(missing) != 0 {
		t.Fatalf("missing fields = %#v, want none", missing)
	}
	want := map[string]string{"subdomain": "acme", "region": ""}
	// Normalization retains the complete declared shape for deterministic persistence.
	if !reflect.DeepEqual(normalized, want) {
		t.Fatalf("normalized input = %#v, want %#v", normalized, want)
	}
}

// TestNormalizeInputReportsMissingRequiredInput confirms incomplete data is
// distinguishable from invalid data so the caller can safely launch the form.
func TestNormalizeInputReportsMissingRequiredInput(t *testing.T) {
	config := normalizeInputTestConfig()
	normalized, missing, err := NormalizeInput(config, map[string]string{"region": "eu"})
	// A required omission is a form-routing result rather than a validation error.
	if err != nil {
		t.Fatalf("NormalizeInput: %v", err)
	}
	// Only the absent required field should be requested from the customer.
	if !reflect.DeepEqual(missing, []string{"subdomain"}) {
		t.Fatalf("missing fields = %#v, want [subdomain]", missing)
	}
	want := map[string]string{"subdomain": "", "region": "eu"}
	// Valid partial context survives so the form does not ask for it again.
	if !reflect.DeepEqual(normalized, want) {
		t.Fatalf("normalized input = %#v, want %#v", normalized, want)
	}
}

// TestNormalizeInputRejectsInvalidDeclaredValue keeps malformed tenant data
// from being converted into a browser fallback that obscures caller errors.
func TestNormalizeInputRejectsInvalidDeclaredValue(t *testing.T) {
	config := normalizeInputTestConfig()
	_, _, err := NormalizeInput(config, map[string]string{"subdomain": "evil.example.com"})
	// Pattern failures must stop before any input or OAuth session is created.
	if err == nil {
		t.Fatal("expected invalid declared resource input to be rejected")
	}
}

// TestNormalizeInputRejectsUndeclaredValue prevents callers from persisting
// arbitrary customer data outside the reviewed resource-input contract.
func TestNormalizeInputRejectsUndeclaredValue(t *testing.T) {
	config := normalizeInputTestConfig()
	_, _, err := NormalizeInput(config, map[string]string{
		"subdomain": "acme",
		"secret":    "must-not-be-stored",
	})
	// Undeclared values fail closed instead of entering routing metadata.
	if err == nil {
		t.Fatal("expected undeclared resource input to be rejected")
	}
}

// normalizeInputTestConfig defines one required routing field and one optional
// context field so tests exercise both collection branches with the same contract.
func normalizeInputTestConfig() *fusedobject.ResourceInputConfig {
	return &fusedobject.ResourceInputConfig{Fields: []fusedobject.ResourceInputField{
		{Name: "subdomain", Required: true, Pattern: `^[a-z0-9-]+$`},
		{Name: "region", Pattern: `^(eu|us)$`},
	}}
}

// TestDiscoveryContractExecutes proves resource discovery is contract-driven rather than provider-dispatched.
func TestDiscoveryContractExecutes(t *testing.T) {
	config := testcontract.ResourceDiscovery()
	if config.Version != 1 || config.Stage != "post_auth" {
		t.Fatalf("discovery lifecycle contract = %#v", config)
	}
	resources, err := Extract([]byte(`[{"id":"project-one","name":"One"}]`), &config)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0].BaseURL != "https://project-one.api.example.test" {
		t.Fatalf("resources = %#v", resources)
	}
}

// TestDiscoverRejectsMutatingOperation keeps the runtime defensive when
// metadata bypasses the normal OpenAPI or Postman import validation.
func TestDiscoverRejectsMutatingOperation(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{ConnectConfig: &fusedobject.ServiceConnectConfig{ResourceDiscovery: &fusedobject.ResourceDiscoveryConfig{}}}
	endpoint := &fusedobject.Endpoint{Method: http.MethodDelete, Path: "/resource"}
	if _, err := Discover(context.Background(), metadata, endpoint, "token", "Bearer"); err == nil {
		t.Fatal("expected mutating discovery operation to be rejected")
	}
}

func TestExtractResourceDiscoveryDeclaredMetadataIsAlignedAndScalar(t *testing.T) {
	config := &fusedobject.ResourceDiscoveryConfig{
		IDPath: "$[*].id", BaseURLPath: "$[*].url", ResourceType: "account",
		AllowedHosts: []string{"api.example.com"},
	}
	resources, err := ExtractWithMetadata([]byte(`[
		{"id":"a","url":"https://api.example.com/a","accountId":"acct-a","region":"eu"},
		{"id":"b","url":"https://api.example.com/b","accountId":"acct-b","region":"us"}
	]`), config, map[string]string{"account_id": "$[*].accountId", "region": "$[*].region"})
	if err != nil {
		t.Fatalf("ExtractWithMetadata: %v", err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(resources[1].Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["account_id"] != "acct-b" || metadata["region"] != "us" {
		t.Fatalf("metadata was not index-aligned: %#v", metadata)
	}
	if _, err := ExtractWithMetadata([]byte(`[{"id":"a","url":"https://api.example.com/a","account":{"id":"secret"}}]`), config, map[string]string{"account": "$[*].account"}); err == nil {
		t.Fatal("expected object metadata to be rejected")
	}
}

func TestDiscoveryBaseURLUsesNamedServer(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{
		BaseURL: "https://default.example.com",
		Servers: fusedobject.Servers{{URL: "https://discovery.example.com", Environment: "discovery"}},
	}
	if got := discoveryBaseURL(metadata, "discovery"); got != "https://discovery.example.com" {
		t.Fatalf("named discovery server = %q", got)
	}
}

// TestExtractResourceDiscovery verifies array-aligned extraction and URL
// rendering for the Jira-style discovery response used by the config guide.
func TestExtractResourceDiscovery(t *testing.T) {
	config := &fusedobject.ResourceDiscoveryConfig{
		IDPath: "$[*].id", NamePath: "$[*].name", ScopesPath: "$[*].scopes",
		BaseURLTemplate: "https://api.atlassian.com/ex/jira/{id}",
		ResourceType:    "jira_site", AllowedHosts: []string{"api.atlassian.com"},
	}
	resources, err := Extract([]byte(`[
		{"id":"cloud-a","name":"Acme","scopes":["read:jira-work"]},
		{"id":"cloud-b","name":"Beta","scopes":["read:jira-user"]}
	]`), config)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(resources) != 2 || resources[1].ProviderID != "cloud-b" || resources[1].Name != "Beta" {
		t.Fatalf("unexpected resources: %#v", resources)
	}
	if resources[0].BaseURL != "https://api.atlassian.com/ex/jira/cloud-a" || len(resources[0].Scopes) != 1 {
		t.Fatalf("unexpected first resource: %#v", resources[0])
	}
}

// TestExtractStaticHostResource permits connected context such as a portal ID
// when dispatch remains on the service's static base URL.
func TestExtractStaticHostResource(t *testing.T) {
	config := &fusedobject.ResourceDiscoveryConfig{
		IDPath: "$[*].id", NamePath: "$[*].name", ResourceType: "portal",
	}
	resources, err := Extract([]byte(`[{"id":"123","name":"Acme"}]`), config)
	if err != nil {
		t.Fatalf("extract static-host resource: %v", err)
	}
	if len(resources) != 1 || resources[0].BaseURL != "" || resources[0].ProviderID != "123" {
		t.Fatalf("unexpected resources: %#v", resources)
	}
}

func TestExtractPreservesLargeNumericResourceIDs(t *testing.T) {
	config := &fusedobject.ResourceDiscoveryConfig{IDPath: "$[*].id", ResourceType: "portal"}
	resources, err := Extract([]byte(`[{"id":9007199254740993}]`), config)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(resources) != 1 || resources[0].ProviderID != "9007199254740993" {
		t.Fatalf("resource ID lost precision: %#v", resources)
	}
}

func TestExtractRejectsStructuredResourceIDs(t *testing.T) {
	config := &fusedobject.ResourceDiscoveryConfig{IDPath: "$[*].id", ResourceType: "portal"}
	if _, err := Extract([]byte(`[{"id":{"tenant":"secret"}}]`), config); err == nil {
		t.Fatal("expected structured resource ID to be rejected")
	}
}

// TestFromInputValidatesTemplateHost proves callers provide only declared
// tenant fields and cannot turn a template into an arbitrary dispatch URL.
func TestFromInputValidatesTemplateHost(t *testing.T) {
	config := &fusedobject.ResourceInputConfig{
		Fields:          []fusedobject.ResourceInputField{{Name: "subdomain", Required: true, Pattern: `^[a-z0-9-]+$`}},
		BaseURLTemplate: "https://{subdomain}.zendesk.com", ResourceType: "tenant",
		AllowedHosts: []string{"*.zendesk.com"},
	}
	resource, err := FromInput(config, map[string]string{"subdomain": "acme"})
	if err != nil {
		t.Fatalf("FromInput: %v", err)
	}
	if resource.BaseURL != "https://acme.zendesk.com" || resource.Name != "acme" {
		t.Fatalf("unexpected resource: %#v", resource)
	}
	if _, err := FromInput(config, map[string]string{"subdomain": "evil.example.com"}); err == nil {
		t.Fatal("expected invalid subdomain to be rejected")
	}
}

// TestMatchDiscoveredInputSelectsExactGrant proves customer input constrains
// the provider grant while preserving its cloud-ID dispatch URL.
func TestMatchDiscoveredInputSelectsExactGrant(t *testing.T) {
	config := matchedResourceInputConfig()
	resources := []Resource{
		{ProviderID: "cloud-a", BaseURL: "https://api.atlassian.com/ex/jira/cloud-a", Metadata: []byte(`{"site_url":"https://acme.atlassian.net"}`)},
		{ProviderID: "cloud-b", BaseURL: "https://api.atlassian.com/ex/jira/cloud-b", Metadata: []byte(`{"site_url":"https://beta.atlassian.net"}`)},
	}
	matched, err := MatchDiscoveredInput(config, map[string]string{"subdomain": "Acme"}, resources)
	// A valid exact grant is required before asserting which routing record survived.
	if err != nil {
		t.Fatalf("MatchDiscoveredInput: %v", err)
	}
	// Matching must preserve the provider cloud ID and API dispatch URL, not the decorative site URL.
	if matched.ProviderID != "cloud-a" || matched.BaseURL != resources[0].BaseURL {
		t.Fatalf("matched resource = %#v", matched)
	}
}

// TestMatchDiscoveredInputRejectsZeroAndMultipleGrants prevents a missing or
// ambiguous provider tenant from producing usable routing state.
func TestMatchDiscoveredInputRejectsZeroAndMultipleGrants(t *testing.T) {
	config := matchedResourceInputConfig()
	noMatch := []Resource{{ProviderID: "cloud-b", Metadata: []byte(`{"site_url":"https://beta.atlassian.net"}`)}}
	// An ungranted customer subdomain cannot create a connected resource.
	if _, err := MatchDiscoveredInput(config, map[string]string{"subdomain": "acme"}, noMatch); !errors.Is(err, ErrDiscoveryInputNoMatch) {
		t.Fatalf("zero-match error = %v", err)
	}
	duplicate := []Resource{
		{ProviderID: "cloud-a", Metadata: []byte(`{"site_url":"https://acme.atlassian.net"}`)},
		{ProviderID: "cloud-a-duplicate", Metadata: []byte(`{"site_url":"https://acme.atlassian.net/"}`)},
	}
	// Canonically duplicate grants cannot select an arbitrary provider row.
	if _, err := MatchDiscoveredInput(config, map[string]string{"subdomain": "acme"}, duplicate); !errors.Is(err, ErrDiscoveryInputAmbiguous) {
		t.Fatalf("multiple-match error = %v", err)
	}
}

// TestMatchDiscoveredInputRejectsUnsafeMetadata fails closed when provider
// metadata cannot be treated as a constrained tenant URL.
func TestMatchDiscoveredInputRejectsUnsafeMetadata(t *testing.T) {
	resources := []Resource{{ProviderID: "cloud-a", Metadata: []byte(`{"site_url":"https://acme.atlassian.net.attacker.test"}`)}}
	// Provider metadata remains inside the same reviewed host allowlist as customer input.
	if _, err := MatchDiscoveredInput(matchedResourceInputConfig(), map[string]string{"subdomain": "acme"}, resources); err == nil {
		t.Fatal("unsafe provider metadata matched")
	}
}

// matchedResourceInputConfig centralizes the exact URL and metadata boundary
// shared by discovery-input matching tests.
func matchedResourceInputConfig() *fusedobject.ResourceInputConfig {
	return &fusedobject.ResourceInputConfig{
		Fields:          []fusedobject.ResourceInputField{{Name: "subdomain", Required: true, Pattern: `^[A-Za-z0-9-]+$`}},
		BaseURLTemplate: "https://{subdomain}.atlassian.net", ResourceType: "jira_site",
		AllowedHosts:   []string{"*.atlassian.net"},
		DiscoveryMatch: &connectionprofile.ResourceInputDiscoveryMatch{MetadataKey: "site_url"},
	}
}

// TestValidateBaseURLRejectsUnsafeRoutes covers scheme, user-info, and host
// allowlist checks before any discovered URL enters Engine storage.
func TestValidateBaseURLRejectsUnsafeRoutes(t *testing.T) {
	tests := []string{
		"http://api.example.com",
		"https://user:pass@api.example.com",
		"https://api.example.com.attacker.test",
	}
	for _, raw := range tests {
		if err := ValidateBaseURL(raw, []string{"api.example.com"}); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
}

// TestDiscoveryHTTPClientBlocksAuthorityChanges covers port changes as well as
// hostname and scheme changes because any authority change can expose a token
// to a different listener.
func TestDiscoveryHTTPClientBlocksAuthorityChanges(t *testing.T) {
	origin, _ := url.Parse("https://api.example.com:443/resources")
	client := discoveryHTTPClient(origin)
	for _, target := range []string{
		"https://other.example.com:443/resources",
		"https://api.example.com:8443/resources",
		"http://api.example.com:443/resources",
	} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if err := client.CheckRedirect(req, []*http.Request{{URL: origin}}); err == nil {
			t.Fatalf("expected redirect to %q to be blocked", target)
		}
	}
	sameAuthority, _ := http.NewRequest(http.MethodGet, "https://api.example.com:443/next", nil)
	if err := client.CheckRedirect(sameAuthority, []*http.Request{{URL: origin}}); err != nil {
		t.Fatalf("same-authority redirect was blocked: %v", err)
	}
}
