package api

import (
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"net/url"
	"testing"
)

// TestAuthorizationScopeMapping binds alternate provider scope fields to the same Engine-owned consent ceiling.
func TestAuthorizationScopeMapping(t *testing.T) {
	auth := fusedobject.AuthConfig{Type: "oauth2", ScopeParameter: "user_scope"}
	flow := fusedobject.OAuth2FlowContract{AuthorizationURL: "https://provider.example/authorize?scope=unreviewed&user_scope=unreviewed"}
	raw, err := buildConnectAuthorizeURL(auth, flow, []string{"app_configurations:write"}, connectClientCredentials{ClientID: "test-client", RedirectURI: "https://engine.example/callback"}, "test-state", "", "test-nonce")
	// A malformed authorization URL must never be presented as a usable consent link.
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	// Static URL defaults cannot broaden either the selected principal or its requested scopes.
	if err != nil || parsed.Query().Get("user_scope") != "app_configurations:write" || parsed.Query().Has("scope") || parsed.Query().Get("state") != "test-state" {
		t.Fatal("scope mapping failed to preserve consent ownership")
	}
	auth.ScopeParameter = "client_id"
	_, err = buildConnectAuthorizeURL(auth, flow, nil, connectClientCredentials{}, "", "", "")
	// Provider metadata cannot repurpose an Engine-owned security parameter as a scope alias.
	if err == nil {
		t.Fatal("reserved scope alias accepted")
	}
}
