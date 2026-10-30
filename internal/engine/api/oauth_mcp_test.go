package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type mcpAudienceFixture struct {
	resource string
	err      error
}

// OAuthMCPResource simulates the live audience lookup without returning any token material.
func (f mcpAudienceFixture) OAuthMCPResource(context.Context, string) (string, error) {
	return f.resource, f.err
}

// TestMCPAuthDiscovery verifies unauthenticated discovery, audience isolation, and existing execution-token compatibility.
func TestMCPAuthDiscovery(t *testing.T) {
	path := "/mcp/" + uuid.NewString()
	resource := "https://engine.example" + path
	tests := []struct {
		name, token, audience string
		downstream, status    int
	}{
		{"discover", "", resource, 200, 401},
		{"manual token", "manual-token", resource, 200, 200},
		{"OAuth same resource", "foat_live", resource, 200, 200},
		{"OAuth other resource", "foat_live", resource + "/other", 200, 401},
		{"revoked runtime token", "manual-token", resource, 401, 401},
	}
	// Every failure must advertise the trusted configured origin rather than the request Host.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := MCPAuthDiscovery("https://engine.example", mcpAudienceFixture{resource: test.audience})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.downstream) }))
			r := httptest.NewRequest(http.MethodPost, "https://attacker.example"+path, nil)
			// Set a bearer only for authenticated cases so discovery exercises a genuinely anonymous request.
			if test.token != "" {
				r.Header.Set("Authorization", "Bearer "+test.token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			// Authentication failures must stop before downstream execution.
			if w.Code != test.status {
				t.Fatalf("status %d", w.Code)
			}
			// Every unauthorized response must advertise the configured origin, never the caller Host.
			if test.status == 401 && (!strings.Contains(w.Header().Get("WWW-Authenticate"), "https://engine.example/.well-known/oauth-protected-resource"+path) || strings.Contains(w.Header().Get("WWW-Authenticate"), "attacker")) {
				t.Fatalf("challenge %q", w.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

// TestMCPConsentSelection rejects empty, unknown, duplicate, and oversized action grants.
func TestMCPConsentSelection(t *testing.T) {
	services := []mcpOAuthServiceChoice{{Index: 0, Operations: []string{"read", "write"}}, {Index: 1, Operations: []string{"send"}}}
	// Exercise each malformed policy independently of the happy path.
	for _, selection := range [][]string{nil, {"delete"}, {"read", "read"}, make([]string, 1001)} {
		// None of these inputs may be normalized into broader valid access.
		if _, err := selectedMCPOperations(selection, services); err == nil {
			t.Fatalf("accepted invalid selection of length %d", len(selection))
		}
	}
	selected, err := selectedMCPOperations([]string{"read"}, services)
	// Selecting an action must authorize only its owning service.
	if err != nil || !selected[0] || selected[1] {
		t.Fatalf("selection %+v %v", selected, err)
	}
}

// TestMCPConsentBindingRejectsForeignAccount proves form values cannot override the authorized connection catalogue.
func TestMCPConsentBindingRejectsForeignAccount(t *testing.T) {
	service := mcpOAuthServiceChoice{Index: 0, Connections: []store.OAuthMCPConnectionChoice{{Value: "approved", AuthName: "Google", EndUserRef: "customer-a"}}}
	r := httptest.NewRequest(http.MethodPost, "/mcp/oauth/consent", nil)
	r.PostForm = url.Values{"account_0": {"foreign"}}
	// Opaque account values outside the live catalogue must be rejected.
	if _, err := submittedMCPBinding(r, service); err == nil {
		t.Fatal("foreign account accepted")
	}
	r.PostForm.Set("account_0", "approved")
	binding, err := submittedMCPBinding(r, service)
	// A valid choice must retain its authenticated scheme identity.
	if err != nil || binding.AuthName != "Google" {
		t.Fatalf("binding %+v %v", binding, err)
	}
}

// TestMCPConsentRendering checks escaped action labels, unselected defaults, and an account picker with no credential values.
func TestMCPConsentRendering(t *testing.T) {
	page := mcpOAuthConsentPage{Name: "Customer support", Resource: "https://engine.example/mcp/" + uuid.NewString(), AppID: uuid.NewString(), Services: []mcpOAuthServiceChoice{{Index: 0, Name: "Gmail", Connected: true, Operations: []string{"Read messages", "Send email <script>"}, Connections: []store.OAuthMCPConnectionChoice{{Value: "opaque", Label: "Support inbox"}}}, {Index: 1, Name: "Jira", Operations: []string{"Search issues", "Create issue"}}}}
	var output bytes.Buffer
	// Template failures must not be mistaken for a safe but empty consent view.
	if err := oauthConsentTemplate.Execute(&output, oauthConsentPage{MCP: &page, Branding: hostedConnectBranding{ConnectBranding: store.DefaultConnectBranding()}, ClientName: "Claude", FormAction: "/mcp/oauth/consent", ClientID: "client", CodeChallengeMethod: "S256"}); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	// Labels must remain escaped, and no action may be pre-approved.
	if strings.Contains(html, "<script>") || strings.Contains(html, " checked") || !strings.Contains(html, `<select id="account_0"`) || !strings.Contains(html, "Support inbox") {
		t.Fatal("unsafe or incomplete consent form")
	}
	// An opt-in temporary preview supports real browser layout checks without committing generated HTML.
	if path := os.Getenv("FUSED_MCP_CONSENT_PREVIEW"); path != "" {
		// An explicitly requested preview must contain the same bytes asserted above.
		if err := os.WriteFile(path, output.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

type mcpTokenFixture struct {
	mcpOAuthService
	resource string
	request  oauthprovider.TokenRequest
}

// TokenMCP captures only the request fields under test and returns synthetic token material.
func (f *mcpTokenFixture) TokenMCP(_ context.Context, req oauthprovider.TokenRequest, resource string) (oauthprovider.TokenResponse, error) {
	f.resource = resource
	f.request = req
	return oauthprovider.TokenResponse{AccessToken: "test-access", RefreshToken: "test-refresh", TokenType: "Bearer", ExpiresIn: 60, Scope: []string{store.OAuthMCPScope}}, nil
}

// TestMCPTokenHTTP requires one canonical resource and forwards it unchanged to the protocol service.
func TestMCPTokenHTTP(t *testing.T) {
	f := &mcpTokenFixture{}
	h := &mcpOAuthHandler{service: f, publicURL: "https://engine.example"}
	resource := "https://engine.example/mcp/" + uuid.NewString()
	// Cover missing, cross-origin, and noncanonical resource indicators.
	for _, audience := range []string{"", resource, "https://other.example/mcp/" + uuid.NewString(), resource + "?other=1"} {
		form := url.Values{"resource": {audience}, "grant_type": {"refresh_token"}, "client_id": {"client"}, "refresh_token": {"refresh"}}
		r := httptest.NewRequest(http.MethodPost, "/mcp/oauth/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		oauthTokenHandler(h.exchangeToken)(w, r)
		// Only the trusted canonical resource may reach token issuance.
		if audience == resource {
			// A valid request must preserve both audience and client identity.
			if w.Code != 200 || f.resource != resource || f.request.ClientID != "client" {
				t.Fatalf("valid exchange %d", w.Code)
			}
			// Malformed audiences must fail locally before token issuance.
		} else if w.Code != 400 {
			t.Fatalf("invalid resource status %d", w.Code)
		}
	}
	var body map[string]any
	w := httptest.NewRecorder()
	oauthIssuerMetadataHandler(h.publicURL+"/mcp/oauth", h.publicURL+"/mcp/oauth", []string{store.OAuthMCPScope}, h.publicURL+"/mcp/oauth/register")(w, httptest.NewRequest(http.MethodGet, "/", nil))
	// Discovery must be valid JSON usable by an OAuth client.
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// Automatic client setup depends on an advertised registration endpoint.
	if body["registration_endpoint"] != "https://engine.example/mcp/oauth/register" {
		t.Fatal("DCR missing from metadata")
	}
	// Lookup failure must never admit a previously issued credential.
	if mcpOAuthAudienceMatches(t.Context(), mcpAudienceFixture{err: errors.New("revoked")}, "foat_test", resource) {
		t.Fatal("revoked grant admitted")
	}
}
