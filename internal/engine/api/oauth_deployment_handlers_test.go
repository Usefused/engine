package api

import (
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDeploymentRegistrationAndConsentNamesManagement requires explicit app-only management consent on the actual wire and page.
func TestDeploymentRegistrationAndConsentNamesManagement(t *testing.T) {
	service := &runtimeHTTPService{&registrationTestService{}}
	handler := &runtimeOAuthHandler{service: service}
	body, err := json.Marshal(map[string]any{"name": "Support", "redirect_uri": "http://127.0.0.1:1234/callback", "manifest": runtimeManifestFixture, "management": true})
	require.NoError(t, err)
	response := httptest.NewRecorder()
	handler.register(response, httptest.NewRequest(http.MethodPost, "/oauth/runtime/register", strings.NewReader(string(body))))
	require.Equal(t, 201, response.Code)
	require.Equal(t, []string{store.OAuthDeploymentScope}, service.request.Scopes)
	manifest, err := parseRuntimeManifest([]byte(runtimeManifestFixture))
	require.NoError(t, err)
	page := &runtimeConsentPage{Management: true, Name: manifest.Name, Version: manifest.Version, Digest: store.RuntimeManifestDigest([]byte(runtimeManifestFixture)), Services: manifest.Services}
	response = httptest.NewRecorder()
	renderOAuthConsentPage(response, httptest.NewRequest(http.MethodGet, "/oauth/runtime/authorize", nil), &mcpConsentHTTPStore{}, oauthprovider.AuthorizeResult{Client: store.OAuthClient{Name: "Support"}, Scope: []string{store.OAuthDeploymentScope}}, oauthprovider.AuthorizeRequest{ClientID: "client", RedirectURI: "http://127.0.0.1:1234/callback"}, nil, page)
	require.Contains(t, response.Body.String(), "publish versions and issue execution credentials for this application only")
	require.Contains(t, response.Body.String(), "app.api.deploy")
	require.NotContains(t, response.Body.String(), "access_token")
}

// TestDeploymentManifestCannotExpandConsent rejects app, provider-version and operation changes before a plan exists.
func TestDeploymentManifestCannotExpandConsent(t *testing.T) {
	original, err := parseRuntimeManifest([]byte(runtimeManifestFixture))
	require.NoError(t, err)
	next, err := parseRuntimeManifest([]byte(strings.Replace(runtimeManifestFixture, `"version":"1.0.0"`, `"version":"2.0.0"`, 1)))
	require.NoError(t, err)
	require.True(t, deploymentManifestWithin(next, original))
	for _, raw := range []string{strings.Replace(runtimeManifestFixture, `"name":"support"`, `"name":"another"`, 1), strings.Replace(runtimeManifestFixture, `"version":"v1"`, `"version":"v2"`, 1), strings.Replace(runtimeManifestFixture, `"postMessage"`, `"deleteMessage"`, 1)} {
		// Every candidate is valid syntax but exceeds one dimension of the approved authority.
		next, err := parseRuntimeManifest([]byte(raw))
		require.NoError(t, err)
		require.False(t, deploymentManifestWithin(next, original))
	}
}
