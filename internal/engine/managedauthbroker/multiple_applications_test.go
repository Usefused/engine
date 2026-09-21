package managedauthbroker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/engine/webhookrelay"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/managedpublication"
	"github.com/Usefused/engine/internal/testoauth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestMultipleManagedApplications isolates two independently owned apps using one unchanged provider auth scheme.
func TestMultipleManagedApplications(t *testing.T) {
	repository, pool, account := installationTestStore(t)
	_, tokens := enrollTestInstallation(t, repository, account)
	installation, err := repository.GetByAccessToken(t.Context(), hashCredential(tokens.AccessToken))
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), installationContextKey{}, installation)
	foreign := publicationConsumerContext(t, pool)
	ownerB := foreign.Value(installationContextKey{}).(Installation).RegistryAccountID
	var calls atomic.Int32
	provider := applicationProvider(t, &calls)
	catalog := NewCatalogStore(pool)
	key := []byte("01234567890123456789012345678901")
	serviceID := uuid.New()
	auth := fusedobject.AuthConfig{Type: "oauth2", TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost}
	auth.Name = "oauth"
	auth.OAuth2Flows = fusedobject.OAuth2Flows{"authorizationCode": {TokenURL: provider.URL}}
	bucketA, versionA := testoauth.Seed(t, pool, serviceID, auth, key, "client-a", "secret-a")
	bucketB, versionB := testoauth.Seed(t, pool, serviceID, auth, key, "client-b", "secret-b")
	a := Registration{ApplicationID: uuid.NewString(), Name: "Team A app", OwnerAccountID: &account, BucketID: bucketA, ServiceVersionID: versionA, FlowName: "authorizationCode"}
	b := Registration{ApplicationID: uuid.NewString(), Name: "Team B app", OwnerAccountID: &ownerB, BucketID: bucketB, ServiceVersionID: versionB, FlowName: "authorizationCode"}
	require.NoError(t, catalog.PublishRegistration(ctx, serviceID, "oauth", a, key))
	// New applications are private even to the owner's enrolled Engine until explicitly granted.
	_, err = catalog.GetProviderApp(ctx, serviceID, "oauth", key, a.ApplicationID)
	require.ErrorIs(t, err, ErrProviderAppNotFound)
	audience := []managedpublication.Audience{{AccountID: account, EngineInstallationID: installation.EngineInstallationID}}
	a.AllowedConsumers, b.AllowedConsumers = audience, audience
	require.NoError(t, catalog.PublishRegistration(ctx, serviceID, "oauth", a, key))
	require.NoError(t, catalog.PublishRegistration(ctx, serviceID, "oauth", b, key))
	app, err := catalog.GetProviderApp(ctx, serviceID, "oauth", key, a.ApplicationID)
	require.NoError(t, err)
	require.Equal(t, "client-a", app.ClientID)
	_, err = catalog.GetProviderApp(foreign, serviceID, "oauth", key, a.ApplicationID)
	require.ErrorIs(t, err, ErrProviderAppNotFound)
	// Sharing the granted consumer's Registry account is insufficient: the Engine identity must also match.
	_, siblingTokens := enrollTestInstallation(t, repository, account)
	sibling, err := repository.GetByAccessToken(t.Context(), hashCredential(siblingTokens.AccessToken))
	require.NoError(t, err)
	_, err = catalog.GetProviderApp(context.WithValue(t.Context(), installationContextKey{}, sibling), serviceID, "oauth", key, a.ApplicationID)
	require.ErrorIs(t, err, ErrProviderAppNotFound)
	_, err = catalog.GetProviderApp(context.Background(), serviceID, "oauth", key, a.ApplicationID)
	require.ErrorIs(t, err, ErrProviderAppNotFound)
	_, err = catalog.GetProviderApp(ctx, serviceID, "oauth", key)
	require.ErrorIs(t, err, ErrProviderAppNotFound) // Absence of a default never chooses the first named app.
	connect, err := NewConnectService(catalog, key, provider.Client())
	require.NoError(t, err)
	connect.Proof = &webhookrelay.ProofStore{DB: pool}
	router := chi.NewRouter()
	MountConnectRoutes(router, connect, repository)
	// Plain OAuth responses need no webhook claims or Slack-specific metadata.
	for _, registration := range []Registration{a, b} {
		result := applicationRequest(t, router, tokens.AccessToken, serviceID, registration.ApplicationID, "exchange", `{"code":"code","redirect_uri":"https://consumer.example/callback"}`)
		require.Equal(t, 200, result.Code)
		require.NotContains(t, result.Body.String(), "secret")
	}
	refreshed, err := connect.Refresh(ctx, serviceID, "oauth", "https://consumer.example/callback", fusedobject.AuthConfig{}, fusedobject.OAuth2FlowContract{}, "refresh-client-a", a.ApplicationID)
	require.NoError(t, err)
	require.Equal(t, "access-client-a", refreshed.AccessToken)
	// A token from app A cannot be refreshed with app B's pair.
	_, err = connect.Refresh(ctx, serviceID, "oauth", "https://consumer.example/callback", fusedobject.AuthConfig{}, fusedobject.OAuth2FlowContract{}, "refresh-client-a", b.ApplicationID)
	require.Error(t, err)
	assertApplicationRevocation(t, catalog, connect, ctx, foreign, serviceID, a, key, &calls)
	// An existing application identity cannot be retargeted, even by publishing another valid bucket.
	a.BucketID, a.ServiceVersionID = b.BucketID, b.ServiceVersionID
	require.Error(t, catalog.PublishRegistration(ctx, serviceID, "oauth", a, key))
	// Removing a broken app's bucket must not prevent operators from withdrawing its remaining audiences.
	_, err = pool.Exec(t.Context(), `DELETE FROM fused_buckets WHERE id=$1`, b.BucketID)
	require.NoError(t, err)
	admin := chi.NewRouter()
	MountAdminRoutes(admin, catalog, key, "operator-secret")
	request := httptest.NewRequest(http.MethodDelete, "/managed-auth/broker/admin/apps/"+serviceID.String()+"/oauth/applications/"+b.ApplicationID, nil)
	request.Header.Set("Authorization", "Bearer operator-secret")
	result := httptest.NewRecorder()
	admin.ServeHTTP(result, request)
	require.Equal(t, http.StatusNoContent, result.Code)
	var withdrawn bool
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT NOT allow_all_enrolled AND allowed_consumers='[]'::jsonb FROM fused_oauth_publications WHERE service_id=$1 AND application_id=$2`, serviceID, b.ApplicationID).Scan(&withdrawn))
	require.True(t, withdrawn)
	require.NoError(t, catalog.RevokeRegistrationAccess(ctx, serviceID, "oauth", b.ApplicationID))

}

// applicationProvider verifies credential identity at a real TLS token endpoint without external account access.
func applicationProvider(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		client := r.PostForm.Get("client_id")
		secret := map[string]string{"client-a": "secret-a", "client-b": "secret-b"}[client]
		// Both credential and refresh-token ownership must match the selected provider app.
		if secret == "" || secret != r.PostForm.Get("client_secret") || (r.PostForm.Get("grant_type") == "refresh_token" && r.PostForm.Get("refresh_token") != "refresh-"+client) {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-" + client, "refresh_token": "refresh-" + client, "token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(server.Close)
	return server
}

// applicationRequest exercises selector parsing and live installation middleware, not just direct catalog reads.
func applicationRequest(t *testing.T, router http.Handler, token string, service uuid.UUID, application, action, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/managed-auth/broker/connect/"+service.String()+"/oauth/applications/"+application+"/"+action, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	result := httptest.NewRecorder()
	router.ServeHTTP(result, req)
	return result
}

// assertApplicationRevocation proves audience withdrawal blocks every credential-bearing operation before provider traffic.
func assertApplicationRevocation(t *testing.T, catalog *CatalogStore, connect *ConnectService, ctx, foreign context.Context, service uuid.UUID, a Registration, key []byte, calls *atomic.Int32) {
	t.Helper()
	before := calls.Load()
	_, err := connect.Exchange(foreign, service, "oauth", "https://consumer.example/callback", fusedobject.AuthConfig{}, fusedobject.OAuth2FlowContract{}, "code", "", a.ApplicationID)
	require.Error(t, err)
	a.AllowedConsumers = nil
	require.NoError(t, catalog.PublishRegistration(ctx, service, "oauth", a, key))
	_, err = connect.ClientID(ctx, service, "oauth", a.ApplicationID)
	require.Error(t, err)
	_, err = connect.Exchange(ctx, service, "oauth", "https://consumer.example/callback", fusedobject.AuthConfig{}, fusedobject.OAuth2FlowContract{}, "code", "", a.ApplicationID)
	require.Error(t, err)
	_, err = connect.Refresh(ctx, service, "oauth", "https://consumer.example/callback", fusedobject.AuthConfig{}, fusedobject.OAuth2FlowContract{}, "refresh-client-a", a.ApplicationID)
	require.Error(t, err)
	require.Equal(t, before, calls.Load())
}

// TestNamedPublicationStillRequiresOperatorKey rejects an enrolled consumer before any catalog mutation or credential read.
func TestNamedPublicationStillRequiresOperatorKey(t *testing.T) {
	router := chi.NewRouter()
	MountAdminRoutes(router, nil, make([]byte, 32), "operator-secret")
	request := httptest.NewRequest(http.MethodPut, "/managed-auth/broker/admin/apps/"+uuid.NewString()+"/oauth", strings.NewReader(`{"managed_application_id":"729ea172-5512-4f37-b202-31084e2d2766","name":"consumer app"}`))
	request.Header.Set("Authorization", "Bearer consumer-installation-token")
	result := httptest.NewRecorder()
	router.ServeHTTP(result, request)
	require.Equal(t, http.StatusUnauthorized, result.Code)
}
