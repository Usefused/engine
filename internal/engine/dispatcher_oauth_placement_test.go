package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
)

// TestOAuthTokenPlacementDispatch exercises real HTTP delivery and preserves default OAuth behavior.
func TestOAuthTokenPlacementDispatch(t *testing.T) {
	for _, test := range []struct {
		name, header, value string
		placement           *authrouting.OAuthTokenPlacement
	}{
		{name: "default", header: "Authorization", value: "Bearer live-token"},
		{name: "custom raw", header: "X-Provider-Token", value: "live-token", placement: &authrouting.OAuthTokenPlacement{Location: "header", Name: "X-Provider-Token", Format: "raw"}},
		{name: "custom bearer", header: "X-Provider-Token", value: "Bearer live-token", placement: &authrouting.OAuthTokenPlacement{Location: "header", Name: "X-Provider-Token", Format: "bearer"}},
	} {
		// Each request observes wire headers rather than merely checking a field mapping.
		t.Run(test.name, func(t *testing.T) {
			// The local provider inspects the fully serialized authentication headers.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The selected header receives the current connection credential.
				if r.Header.Get(test.header) != test.value {
					t.Errorf("unexpected token placement")
				}
				// Custom placement must not also emit the default Bearer token.
				if test.header != "Authorization" && r.Header.Get("Authorization") != "" {
					t.Error("token also sent in default header")
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			req, err := http.NewRequest(http.MethodGet, server.URL, nil)
			// Test setup errors must not masquerade as dispatch failures.
			if err != nil {
				t.Fatal(err)
			}
			auth := models.AuthConfig{Name: "oauth", Type: "oauth2", OAuthTokenPlacement: test.placement}
			// OAuth injection is shared by direct API, SDK, and MCP dispatch.
			if err := applySelectedAuthChecked(req, models.AuthConfigs{auth}, map[string]any{"oauth": "live-token"}); err != nil {
				t.Fatal(err)
			}
			resp, err := oauthPlacementClient(server.Client(), models.AuthConfigs{auth}).Do(req)
			// A valid provider route must execute successfully through the scoped client.
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		})
	}
}

// TestOAuthTokenPlacementBlocksCrossOriginRedirect prevents net/http from forwarding a custom credential header to another authority.
func TestOAuthTokenPlacementBlocksCrossOriginRedirect(t *testing.T) {
	var reached atomic.Bool
	// A redirected host must never receive the credential-bearing request.
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Store(true); w.WriteHeader(200) }))
	defer destination.Close()
	// The provider redirects to a distinct authority to exercise the leak boundary.
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer source.Close()
	auth := models.AuthConfig{Name: "oauth", Type: "oauth2", OAuthTokenPlacement: &authrouting.OAuthTokenPlacement{Location: "header", Name: "X-Provider-Token", Format: "raw"}}
	req, _ := http.NewRequest(http.MethodGet, source.URL, nil)
	// Admission must succeed before the redirect policy is exercised.
	if err := applyOAuth(req, auth, map[string]any{"oauth": "live-token"}); err != nil {
		t.Fatal(err)
	}
	_, err := oauthPlacementClient(source.Client(), models.AuthConfigs{auth}).Do(req)
	// Both the bounded error and absence of a destination request prove no token was forwarded.
	if err == nil || !strings.Contains(err.Error(), "cross-origin") || reached.Load() {
		t.Fatalf("redirect result: reached=%v err=%v", reached.Load(), err)
	}
}

// TestOAuthTokenPlacementHeaderOwnership prevents order-dependent credential overwrites in AND authentication.
func TestOAuthTokenPlacementHeaderOwnership(t *testing.T) {
	oauth := models.AuthConfig{Name: "oauth", Type: "oauth2", OAuthTokenPlacement: &authrouting.OAuthTokenPlacement{Location: "header", Name: "X-Provider-Token", Format: "raw"}}
	apiKey := models.AuthConfig{Name: "key", Type: "apiKey", Location: "header", KeyName: "x-provider-token"}
	for _, auths := range []models.AuthConfigs{{oauth, apiKey}, {apiKey, oauth}} {
		req := httptest.NewRequest(http.MethodGet, "https://provider.example", nil)
		err := applySelectedAuthChecked(req, auths, map[string]any{"oauth": "live-token", "key": "private-key"})
		// Collisions fail before either secret is injected, regardless of scheme ordering or header case.
		if err == nil || !strings.Contains(err.Error(), "header_collision") || len(req.Header) != 0 {
			t.Fatalf("header collision was not rejected safely: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "https://provider.example", nil)
	basic := models.AuthConfig{Name: "basic", Type: "http", Scheme: "basic"}
	// Separate header destinations remain valid for providers requiring two simultaneous credentials.
	if err := applySelectedAuthChecked(req, models.AuthConfigs{oauth, basic}, map[string]any{"oauth": "live-token", "basic_username": "user", "basic_password": "pass"}); err != nil {
		t.Fatal(err)
	}
	// Custom OAuth must preserve the other scheme's Authorization header.
	if req.Header.Get("X-Provider-Token") != "live-token" || !strings.HasPrefix(req.Header.Get("Authorization"), "Basic ") {
		t.Fatal("independent auth headers were not preserved")
	}
}
