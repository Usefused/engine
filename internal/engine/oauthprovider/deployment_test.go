package oauthprovider

import (
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/stretchr/testify/require"
	"testing"
)

// TestDeploymentConsentDoesNotReuseExecutionConsent requires an explicit app-management scope and standard token JSON.
func TestDeploymentConsentDoesNotReuseExecutionConsent(t *testing.T) {
	client := confidentialClient()
	client.AllowedScopes = []string{store.OAuthDeploymentScope}
	repository := &fakeOAuthStore{client: client, consentFound: true, consent: store.OAuthConsent{GrantedScope: client.AllowedScopes}}
	service, _ := newTestService(t, repository)
	request := authorizeRequest(client)
	request.Scope = client.AllowedScopes
	result, err := service.AuthorizeRuntime(t.Context(), browserActor(t), request)
	require.NoError(t, err)
	require.True(t, result.RequiresConsent)
	_, err = service.Authorize(t.Context(), browserActor(t), request)
	require.Error(t, err)
	raw, err := json.Marshal(RuntimeTokenResponse{RuntimeManagementTokens: runtimeManagementTokens(TokenResponse{AccessToken: "access", RefreshToken: "refresh", TokenType: "Bearer", ExpiresIn: 3600, Scope: []string{store.OAuthDeploymentScope}}), RuntimeToken: "frt_separate"})
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Equal(t, "app.api.deploy", wire["scope"])
	require.Equal(t, "access", wire["access_token"])
	require.Equal(t, "frt_separate", wire["runtime_token"])
}

// TestDeploymentTokenOmitsInternalCeiling keeps full contract proofs out of public OAuth responses while preserving stored grant data.
func TestDeploymentTokenOmitsInternalCeiling(t *testing.T) {
	grant := store.OAuthRuntimeGrant{ContractBindings: []models.SDKContractBinding{{SourceHash: "private-proof"}}}
	raw, err := json.Marshal(RuntimeTokenResponse{OAuthRuntimeGrant: grant, RuntimeToken: "frt_separate"})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "contract_bindings")
	require.NotContains(t, string(raw), "private-proof")
	require.Len(t, grant.ContractBindings, 1)
}
