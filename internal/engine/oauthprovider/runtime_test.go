package oauthprovider

import (
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/stretchr/testify/require"
)

// TestRuntimeOAuthAlwaysReviewsManifest isolates API provisioning from prior consent and management credentials.
func TestRuntimeOAuthAlwaysReviewsManifest(t *testing.T) {
	client := confidentialClient()
	client.AllowedScopes = []string{store.OAuthRuntimeScope}
	repository := &fakeOAuthStore{client: client, consentFound: true, consent: store.OAuthConsent{GrantedScope: client.AllowedScopes}}
	service, _ := newTestService(t, repository)
	request := authorizeRequest(client)
	request.Scope = client.AllowedScopes
	result, err := service.AuthorizeRuntime(t.Context(), browserActor(t), request)
	require.NoError(t, err)
	require.True(t, result.RequiresConsent)
	require.Empty(t, repository.recordedCode.CodeHash)
	_, err = service.Authorize(t.Context(), browserActor(t), request)
	require.Error(t, err)
	request.RedirectURI = "https://attacker.example/callback"
	_, err = service.AuthorizeRuntime(t.Context(), browserActor(t), request)
	require.Error(t, err)
	_, err = service.TokenRuntime(t.Context(), TokenRequest{GrantType: "refresh_token"})
	require.ErrorIs(t, err, ErrUnsupportedGrantType)
}
