package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/managedauthbroker"
	"github.com/Usefused/engine/internal/engine/managedauthclient"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/testoauth"
)

// fixedTicketMinter always returns the same ticket, which the registry
// fixture below always accepts -- this test is only exercising the broker
// and connect-runtime code, not Registry's own ticket issuance (that is
// covered for real in backend/internal/registry/api/managed_auth_enrollment_e2e_test.go).
type fixedTicketMinter struct{}

func (fixedTicketMinter) MintManagedAuthEnrollmentTicket(context.Context) (string, error) {
	return "fixed-ticket", nil
}

// TestStartConnectSessionUsesManagedAppOnlyWhenExplicitlyReferenced proves the
// explicit-only resolver branch in resolveConnectRuntimeConfigForVersion: a
// session start that supplies the fused auth_ref succeeds against a real
// (Postgres-backed) managed-auth broker, and the authorize URL carries the
// broker-issued client_id, never a secret. Managed auth is never a fallback.
func TestStartConnectSessionUsesManagedAppOnlyWhenExplicitlyReferenced(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := db.InitEnginePostgres(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("initialize Engine database: %v", err)
	}
	t.Cleanup(pool.Close)

	fixture := newConnectRuntimeFixture(t)
	// No workspace-owned application registration exists for this bucket/service/authName.
	fixture.store.applicationSecrets = nil
	// The fused reference resolves its service key through the same local
	// generation-contract lookup as a plain bucket reference; the fixture maps
	// "gmail" to the target service, which is also the broker catalogue key.
	fixture.store.sourceServiceID = fixture.serviceID
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_oauth_publications WHERE service_id = $1`, fixture.serviceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_managed_auth_installations`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_managed_auth_broker_credential`)
	})

	// -- Real broker: register a managed app for exactly this fixture's service/authName.
	catalog := managedauthbroker.NewCatalogStore(pool)
	masterKey := []byte("01234567890123456789012345678901")
	// The provider registration reuses an ordinary bucket pair and exact service contract.
	auth := fusedobject.AuthConfig{Name: "bearerAuth", Type: "oauth2", TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost, OAuth2Flows: fusedobject.OAuth2Flows{"authorizationCode": {TokenURL: "https://provider.example/token"}}}
	bucket, version := testoauth.Seed(t, pool, fixture.serviceID, auth, masterKey, "managed-client-id", "managed-client-secret")
	if err := catalog.PublishRegistration(t.Context(), fixture.serviceID, "bearerAuth", managedauthbroker.Registration{BucketID: bucket, ServiceVersionID: version, FlowName: "authorizationCode"}, masterKey); err != nil {
		t.Fatal(err)
	}

	installs := managedauthbroker.NewStore(pool)
	registryFixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"account_id": uuid.New().String(), "installation_id": uuid.New().String(), "expires_at": time.Now().Add(5 * time.Minute).Format(time.RFC3339Nano)})
	}))
	t.Cleanup(registryFixture.Close)
	verifier := managedauthbroker.NewRegistryTicketVerifier(registryFixture.URL+"/graphql", nil)
	brokerService, err := managedauthbroker.NewService(installs, verifier)
	if err != nil {
		t.Fatalf("NewService (broker): %v", err)
	}
	connectService, err := managedauthbroker.NewConnectService(catalog, masterKey, nil)
	if err != nil {
		t.Fatalf("NewConnectService: %v", err)
	}
	brokerRouter := chi.NewRouter()
	managedauthbroker.MountRoutes(brokerRouter, brokerService)
	managedauthbroker.MountConnectRoutes(brokerRouter, connectService, installs)
	brokerServer := httptest.NewServer(brokerRouter)
	t.Cleanup(brokerServer.Close)

	// -- Real client: enroll with the real broker above, then build the
	// ConnectClient the connect-runtime resolver falls back to.
	clientStore := managedauthclient.NewStore(pool)
	clientService, err := managedauthclient.NewService(clientStore, fixedTicketMinter{}, managedauthclient.NewHTTPBrokerClient(brokerServer.URL, nil), masterKey)
	if err != nil {
		t.Fatalf("NewService (client): %v", err)
	}
	if err := clientService.Enroll(t.Context()); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	managedConnect := managedauthclient.NewConnectClient(brokerServer.URL, clientService, nil)

	router := newControlTestRouter(fixture.store.accountID)
	router.Mount("/workspace", WorkspaceHandler(fixture.store, fixture.verifier, fixture.masterKey, fixture.store, managedConnect, "https://engine.example.com/workspace/connect/callback"))

	appID := attachConnectTestArtifact(&fixture)
	// The fused reference is what selects the managed app; without it this
	// start would fail exactly like any other missing local credential.
	body := `{"end_user_ref":"user_123","created_by_app_id":"` + appID.String() + `","auth_ref":"${fused.bucket.auth.gmail.bearerAuth}"}`
	request := httptest.NewRequest(http.MethodPost, fixture.startPath(), strings.NewReader(body))
	request.Header.Set("X-API-Key", "fsk_test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200 via explicit managed-auth reference, got %d: %s", response.Code, response.Body.String())
	}
	values := authorizeURLValues(t, response.Body.Bytes())
	if values.Get("client_id") != "managed-client-id" {
		t.Fatalf("expected the broker-issued managed client_id in the authorize URL, got %#v", values)
	}
	if strings.Contains(response.Body.String(), "managed-client-secret") {
		t.Fatalf("response must never contain the managed app's client secret: %s", response.Body.String())
	}
}
