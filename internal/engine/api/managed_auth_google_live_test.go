package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/Usefused/engine/internal/engine/managedauthbroker"
	"github.com/Usefused/engine/internal/engine/managedauthclient"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/testoauth"
)

// liveGoogleStore uses fixture workspace admission but production PostgreSQL for every session and user credential.
type liveGoogleStore struct {
	*connectAdminMockStore
	postgres  store.Store
	completed chan uuid.UUID
}

// CreateConnectSession persists the same hashed-state and encrypted-PKCE session as the production callback flow.
func (s *liveGoogleStore) CreateConnectSession(ctx context.Context, session store.ConnectSession) (*store.ConnectSession, error) {
	return s.postgres.CreateConnectSession(ctx, session)
}

// GetConnectSessionByStateHash resolves callback state in the consumer database only.
func (s *liveGoogleStore) GetConnectSessionByStateHash(ctx context.Context, hash string) (*store.ConnectSession, error) {
	return s.postgres.GetConnectSessionByStateHash(ctx, hash)
}

// MarkConnectSessionUsed preserves the real single-use callback boundary.
func (s *liveGoogleStore) MarkConnectSessionUsed(ctx context.Context, hash string, at time.Time) error {
	return s.postgres.MarkConnectSessionUsed(ctx, hash, at)
}

// UpsertAuthConnection signals completion only after the production encrypted connection write succeeds.
func (s *liveGoogleStore) UpsertAuthConnection(ctx context.Context, conn store.AuthConnection) (*store.AuthConnection, error) {
	saved, err := s.postgres.UpsertAuthConnection(ctx, conn)
	// A failed persistence attempt must not be reported as a successful consent.
	if err == nil {
		s.completed <- saved.ID
	}
	return saved, err
}

// liveGoogleAuthority stands in only for Registry licensing, which has separate PostgreSQL HTTP coverage.
type liveGoogleAuthority struct{ account, installation uuid.UUID }

// VerifyTicket grants a short approval for this isolated consumer without relying on a deployed Registry build.
func (a liveGoogleAuthority) VerifyTicket(context.Context, string) (managedauthbroker.EnrollmentIdentity, error) {
	return managedauthbroker.EnrollmentIdentity{AccountID: a.account, InstallationID: a.installation, ExpiresAt: time.Now().Add(5 * time.Minute)}, nil
}

// liveGooglePool admits only the explicitly named disposable database and uses the normal Engine schema.
func liveGooglePool(t *testing.T, env, expectedDatabase string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv(env)
	require.Contains(t, dsn, "/"+expectedDatabase+"?", "live tests require their dedicated disposable database")
	pool, err := db.InitEnginePostgres(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// liveGoogleBroker publishes real Google credentials only in the broker's ordinary private bucket.
func liveGoogleBroker(t *testing.T, pool *pgxpool.Pool, serviceID uuid.UUID, auth fusedobject.AuthConfig) (string, uuid.UUID) {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	bucket, version := testoauth.Seed(t, pool, serviceID, auth, key, os.Getenv("FUSED_GOOGLE_OAUTH_ID"), os.Getenv("FUSED_GOOGLE_OAUTH_SECRET"))
	catalog := managedauthbroker.NewCatalogStore(pool)
	require.NoError(t, catalog.PublishRegistration(t.Context(), serviceID, auth.Name, managedauthbroker.Registration{BucketID: bucket, ServiceVersionID: version, FlowName: "authorizationCode"}, key))
	installs := managedauthbroker.NewStore(pool)
	authority := liveGoogleAuthority{account: uuid.New(), installation: uuid.New()}
	service, err := managedauthbroker.NewService(installs, authority)
	require.NoError(t, err)
	connect, err := managedauthbroker.NewConnectService(catalog, key, nil)
	require.NoError(t, err)
	router := chi.NewRouter()
	managedauthbroker.MountRoutes(router, service)
	managedauthbroker.MountConnectRoutes(router, connect, installs)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server.URL, version
}

// TestGoogleManagedAuthLive verifies browser consent, real provider exchange, encrypted customer custody and normal coordinator refresh.
// It is opt-in because Google consent is interactive; Registry/workspace admission are fixtures, while OAuth and both databases are real.
func TestGoogleManagedAuthLive(t *testing.T) {
	// Ordinary test runs never start a browser flow or access a live provider.
	if os.Getenv("FUSED_GOOGLE_LIVE_TEST") != "1" {
		t.Skip("set FUSED_GOOGLE_LIVE_TEST=1 for interactive Google consent")
	}
	require.NotEmpty(t, os.Getenv("FUSED_GOOGLE_OAUTH_ID"), "Google test client ID is required")
	require.NotEmpty(t, os.Getenv("FUSED_GOOGLE_OAUTH_SECRET"), "Google test client secret is required")
	brokerPool := liveGooglePool(t, "FUSED_GOOGLE_BROKER_DATABASE_URL", "fused_google_broker_test")
	consumerPool := liveGooglePool(t, "FUSED_GOOGLE_CONSUMER_DATABASE_URL", "fused_google_consumer_test")
	fixture := newConnectRuntimeFixture(t)
	// Live provider grants use a fresh ephemeral encryption key, never the static unit-test fixture key.
	_, keyErr := rand.Read(fixture.masterKey)
	require.NoError(t, keyErr)
	auth := fusedobject.AuthConfig{Name: "bearerAuth", Type: "oauth2", PKCERequired: true, RefreshTokenRequired: true,
		TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost,
		ExtraAuthParams:         map[string]string{"access_type": "offline", "prompt": "consent"},
		OAuth2Flows:             fusedobject.OAuth2Flows{"authorizationCode": {AuthorizationURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token", Scopes: map[string]string{"openid": "Identity", "email": "Email", "profile": "Profile"}}}}
	brokerURL, version := liveGoogleBroker(t, brokerPool, fixture.serviceID, auth)
	fixture.verifier.serviceMetadata.AuthConfigs = fusedobject.AuthConfigs{auth}
	fixture.verifier.serviceMetadata.ServiceVersionID = version
	consumer := store.NewPostgresStore(consumerPool)
	bucket, err := consumer.CreateBucket(t.Context(), "google-managed-auth-live", false)
	require.NoError(t, err)
	fixture.bucketID, fixture.store.bucketID = bucket.ID, bucket.ID
	fixture.store.sourceServiceID = fixture.serviceID
	fixture.store.applicationSecrets = nil
	_, err = consumer.(store.ServiceContractSnapshotStore).UpsertServiceContractSnapshot(t.Context(), store.ServiceContractSnapshot{ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(), ServiceID: fixture.serviceID, ServiceVersionID: version, Version: "2026-07-01", ServiceMetadata: *fixture.verifier.serviceMetadata})
	require.NoError(t, err)
	client, err := managedauthclient.NewService(managedauthclient.NewStore(consumerPool), fixedTicketMinter{}, managedauthclient.NewHTTPBrokerClient(brokerURL, nil), fixture.masterKey)
	require.NoError(t, err)
	require.NoError(t, client.Enroll(t.Context()))
	managed := managedauthclient.NewConnectClient(brokerURL, client, nil)
	persistent := &liveGoogleStore{connectAdminMockStore: fixture.store, postgres: consumer, completed: make(chan uuid.UUID, 1)}
	router := newControlTestRouter(fixture.store.accountID)
	router.Mount("/workspace", WorkspaceHandler(persistent, fixture.verifier, fixture.masterKey, persistent, managed, "http://localhost:8081/workspace/connect/callback"))
	connectionID := liveGoogleConsent(t, router, fixture.startPath(), persistent.completed)
	liveGoogleVerifyAndRefresh(t, consumer, brokerPool, consumerPool, connectionID, fixture.masterKey, managed)
	require.NoError(t, client.Disable(t.Context()))
	require.Equal(t, managedauthclient.StatusDisabled, client.Status(t.Context()))
	retained, err := consumer.GetAuthConnectionByIDForBuckets(t.Context(), connectionID, []uuid.UUID{bucket.ID})
	require.NoError(t, err)
	require.NotNil(t, retained, "disabling must preserve the customer's provider connection")
	require.NoError(t, client.Enroll(t.Context()))
	t.Log("Google consent, broker exchange, encrypted consumer persistence, userinfo, coordinator refresh, disable and re-enable passed")
}

// liveGoogleConsent listens on the registered loopback callback and emits only a private browser navigation artifact.
func liveGoogleConsent(t *testing.T, router http.Handler, startPath string, completed <-chan uuid.UUID) uuid.UUID {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:8081")
	require.NoError(t, err, "the registered callback port must be available")
	server := &http.Server{Handler: router, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	request := httptest.NewRequest(http.MethodPost, startPath, strings.NewReader(`{"end_user_ref":"google-managed-auth-live","auth_ref":"${fused.bucket.auth.gmail.bearerAuth}","scopes":["openid","email","profile"]}`))
	request.Header.Set("X-API-Key", "fsk_test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, "connect start failed: %s", response.Body.String())
	var result struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.NotEmpty(t, result.AuthorizeURL)
	path := "/private/tmp/fused-google-managed-auth-url"
	require.NoError(t, os.WriteFile(path, []byte(result.AuthorizeURL), 0600))
	t.Cleanup(func() { _ = os.Remove(path) })
	t.Log("Google consent is ready; browser URL saved to the private navigation artifact")
	// Consent remains interactive and bounded; no callback code or provider token is logged.
	select {
	case id := <-completed:
		return id
	case <-time.After(8 * time.Minute):
		t.Fatal("Google consent did not complete within eight minutes")
	case <-t.Context().Done():
		t.Fatal("Google consent test cancelled")
	}
	return uuid.Nil
}

// liveGoogleVerifyAndRefresh proves only the consumer stores provider tokens and uses the existing refresh coordinator.
func liveGoogleVerifyAndRefresh(t *testing.T, consumer store.Store, brokerPool, consumerPool *pgxpool.Pool, id uuid.UUID, key []byte, managed *managedauthclient.ConnectClient) {
	t.Helper()
	refreshStore := consumer.(store.AuthConnectionRefreshStore)
	conn, err := refreshStore.GetAuthConnectionByID(t.Context(), id)
	require.NoError(t, err)
	require.True(t, conn.ManagedAuth)
	require.NotEmpty(t, conn.EncryptedRefreshToken)
	dek, err := store.UnwrapDEK(key, conn.EncryptedDEK)
	require.NoError(t, err)
	access, err := store.DecryptWithDEK(dek, conn.EncryptedAccessToken)
	require.NoError(t, err)
	require.NotEqual(t, access, conn.EncryptedAccessToken)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://openidconnect.googleapis.com/v1/userinfo", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+access)
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var count int
	require.NoError(t, brokerPool.QueryRow(t.Context(), `SELECT count(*) FROM fused_auth_connections`).Scan(&count))
	require.Zero(t, count, "broker must not own the user's connection")
	require.NoError(t, consumerPool.QueryRow(t.Context(), `SELECT count(*) FROM fused_workspace_secrets`).Scan(&count))
	require.Zero(t, count, "consumer must not hold Google application credentials")
	_, err = consumerPool.Exec(t.Context(), `UPDATE fused_auth_connections SET expires_at=NOW()-INTERVAL '1 minute' WHERE id=$1`, id)
	require.NoError(t, err)
	claim, err := refreshStore.TryClaimAuthConnectionRefresh(t.Context(), id, time.Now(), time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, claim)
	coordinator := sandbox.NewAuthRefreshCoordinator(consumer, key, sandbox.WithAuthRefreshManagedConnect(managed))
	result, err := coordinator.RefreshClaimedConnection(t.Context(), *claim)
	require.NoError(t, err)
	require.Equal(t, sandbox.AuthRefreshOutcomeRefreshed, result.Outcome)
}
