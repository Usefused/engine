package connectresource

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/testcontract"
)

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
