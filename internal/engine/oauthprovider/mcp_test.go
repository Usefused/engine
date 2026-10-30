package oauthprovider

import (
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
)

// TestMCPAuthorizeAlwaysReviewsActions prevents coarse prior consent from silently authorizing a different MCP server.
func TestMCPAuthorizeAlwaysReviewsActions(t *testing.T) {
	client := confidentialClient()
	client.AllowedScopes = []string{store.OAuthMCPScope}
	repository := &fakeOAuthStore{client: client, consentFound: true, consent: store.OAuthConsent{GrantedScope: []string{store.OAuthMCPScope}}}
	service, _ := newTestService(t, repository)
	request := authorizeRequest(client)
	request.Scope = []string{store.OAuthMCPScope}
	result, err := service.AuthorizeMCP(t.Context(), browserActor(t), request)
	// Every connection must review its actual server and actions, even with an existing client consent.
	if err != nil || !result.RequiresConsent || repository.recordedCode.CodeHash != "" {
		t.Fatalf("authorize: %+v %v", result, err)
	}
	request.RedirectURI = "https://attacker.example/callback"
	// A registered client must not redirect an authorization code to an unregistered origin.
	if _, err := service.AuthorizeMCP(t.Context(), browserActor(t), request); err == nil {
		t.Fatal("unregistered redirect accepted")
	}
}

// TestMCPRejectsManagementClients verifies that neither issuer can convert the other's client grants.
func TestMCPRejectsManagementClients(t *testing.T) {
	client := confidentialClient()
	service, _ := newTestService(t, &fakeOAuthStore{client: client})
	// A registered client must not redirect an authorization code to an unregistered origin.
	if _, err := service.AuthorizeMCP(t.Context(), browserActor(t), authorizeRequest(client)); err == nil {
		t.Fatal("management client entered runtime issuer")
	}
	client.AllowedScopes = []string{store.OAuthMCPScope}
	service, _ = newTestService(t, &fakeOAuthStore{client: client})
	req := authorizeRequest(client)
	req.Scope = client.AllowedScopes
	// Management authorization must reject the runtime-only scope.
	if _, err := service.Authorize(t.Context(), browserActor(t), req); err == nil {
		t.Fatal("runtime client entered management issuer")
	}
}

// TestMCPRegistrationCeiling verifies standard remote registration is provisional, hashed, and runtime-only.
func TestMCPRegistrationCeiling(t *testing.T) {
	// Exercise every advertised client authentication method.
	for _, method := range []string{"none", "client_secret_basic", "client_secret_post"} {
		t.Run(method, func(t *testing.T) {
			repository := &fakeOAuthStore{}
			service, _ := newTestService(t, repository)
			result, err := service.RegisterMCPClient(t.Context(), MCPRegistration{ClientName: "Claude", RedirectURIs: []string{"https://claude.ai/api/mcp/auth_callback"}, TokenEndpointAuthMethod: method})
			// Registration must remain runtime-only and expire unless browser consent succeeds.
			if err != nil || !store.OAuthMCPOnly(repository.recordedRegister.AllowedScopes) || repository.recordedRegister.ExpiresAt == nil || repository.recordedRegister.ExpiresAt.After(time.Now().Add(time.Hour)) {
				t.Fatalf("registration %v", err)
			}
			assertMCPRegistrationSecret(t, method, result.ClientSecret, repository.recordedRegister.ClientSecretHash)
		})
	}
}

// TestMCPRegistrationRejectsUnsupportedMetadata prevents protocol downgrade and management-scope injection.
func TestMCPRegistrationRejectsUnsupportedMetadata(t *testing.T) {
	// Reject unsupported client contracts before issuing credentials.
	for _, request := range []MCPRegistration{{Scope: "service.read"}, {TokenEndpointAuthMethod: "client_secret_jwt"}, {GrantTypes: []string{"client_credentials"}}, {ResponseTypes: []string{"token"}}} {
		// Unsupported metadata cannot be silently reduced to a different client contract.
		if validateMCPRegistration(request) == nil {
			t.Fatalf("accepted invalid metadata %+v", request)
		}
	}
}

// assertMCPRegistrationSecret checks public clients and one-time confidential secret hashing independently.
func assertMCPRegistrationSecret(t *testing.T, method, secret, storedHash string) {
	t.Helper()
	// Public clients must not acquire an implicit shared secret.
	if method == "none" {
		// Public clients must have neither a returned nor persisted shared secret.
		if secret != "" || storedHash != "" {
			t.Fatal("public client received a secret")
		}
		return
	}
	// Confidential credentials must be independently generated and hash-only at rest.
	if secret == "" || storedHash != hashSecret(secret) {
		t.Fatal("secret was not hashed")
	}
}
