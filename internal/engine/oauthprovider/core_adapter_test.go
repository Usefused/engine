package oauthprovider

import (
	"fmt"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/stretchr/testify/require"
)

// TestSharedCoreConsentRequiresExplicitScope covers the consent POST as well as authorize.
func TestSharedCoreConsentRequiresExplicitScope(t *testing.T) {
	client := confidentialClient()
	repository := &fakeOAuthStore{client: client}
	service, _ := newTestService(t, repository)
	actor := browserActor(t, "service.read")
	_, err := service.Consent(t.Context(), actor, ConsentRequest{
		ClientID: client.ClientID, RedirectURI: client.RedirectURIs[0],
		CodeChallenge: validCodeChallenge, CodeChallengeMethod: "S256",
	})
	// The core's default-to-all behavior must never issue an implicit Fused grant.
	require.ErrorIs(t, err, ErrInvalidScope)
	require.Empty(t, repository.recordedCode.CodeHash)
}

// TestSharedCorePreservesPersistedMetadata covers response fields absent from the pinned core model.
func TestSharedCorePreservesPersistedMetadata(t *testing.T) {
	now := time.Now().UTC()
	expiry := now.Add(7 * time.Minute)
	client := confidentialClient()
	client.ExpiresAt = &expiry
	repository := &fakeOAuthStore{client: client, listClients: []store.OAuthClient{client}}
	service, _ := newTestService(t, repository)
	result, err := service.Authorize(t.Context(), browserActor(t, "service.read"), authorizeRequest(client))
	// Projection through the core must not discard Fused's client expiry.
	require.NoError(t, err)
	require.Equal(t, client, result.Client)
	clients, err := service.ListClients(t.Context())
	require.NoError(t, err)
	require.Equal(t, []store.OAuthClient{client}, clients)

	// Both entry points must report the deadline returned by storage.
	for _, grant := range []string{"authorization_code", "refresh_token"} {
		// Both grant paths must use their own persisted deadline, never the core's default hour.
		t.Run(grant, func(t *testing.T) {
			metadata := store.OAuthTokenMetadata{AccessExpiresAt: expiry, Scope: []string{"service.read"}}
			repository := &fakeOAuthStore{client: client, clientSecret: hashSecret("secret"), exchangeResult: metadata, rotateResult: metadata}
			service, _ := newTestService(t, repository)
			// A fixed response clock makes any accidental default lifetime visible.
			service.now = func() time.Time { return now }
			response, err := service.Token(t.Context(), TokenRequest{
				GrantType: grant, ClientID: client.ClientID, ClientSecret: "secret",
				Code: "code", CodeVerifier: "verifier", RedirectURI: client.RedirectURIs[0], RefreshToken: "refresh",
			})
			require.NoError(t, err)
			require.EqualValues(t, 420, response.ExpiresIn)
			require.Equal(t, metadata.Scope, response.Scope)
			require.NotEmpty(t, response.AccessToken)
		})
	}
}

// TestSharedCorePreservesWrappedGrantErrors prevents storage wrapping from changing protocol denials.
func TestSharedCorePreservesWrappedGrantErrors(t *testing.T) {
	client := confidentialClient()
	// Transaction wrappers must preserve both ordinary denial and replay-detection semantics.
	for _, cause := range []error{store.ErrOAuthGrantDenied, store.ErrOAuthGrantReuseDetected} {
		repository := &fakeOAuthStore{client: client, clientSecret: hashSecret("secret"), rotateErr: fmt.Errorf("transaction: %w", cause)}
		service, _ := newTestService(t, repository)
		response, err := service.Token(t.Context(), TokenRequest{GrantType: "refresh_token", ClientID: client.ClientID, ClientSecret: "secret", RefreshToken: "refresh"})
		// Shared sentinel identity is essential for errors.Is inside the core.
		require.ErrorIs(t, err, ErrInvalidGrant)
		require.Empty(t, response.AccessToken)
	}
}
