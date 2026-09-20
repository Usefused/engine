package managedauthbroker

import (
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/testoauth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// TestAdminRegistrationRequiresCanonicalSource rejects old registration payloads before any database operation.
func TestAdminRegistrationRequiresCanonicalSource(t *testing.T) {
	router := chi.NewRouter()
	MountAdminRoutes(router, nil, make([]byte, 32), "fixture-admin")
	req := httptest.NewRequest(http.MethodPut, "/managed-auth/broker/admin/apps/"+uuid.NewString()+"/OAuth", strings.NewReader(`{"client_id":"fixture-client","client_secret":"fixture-secret"}`))
	req.Header.Set("Authorization", "Bearer fixture-admin")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	// A secret without a trusted endpoint must not reach the catalogue's persistence path.
	if response.Code != http.StatusBadRequest {
		t.Fatalf("registration without canonical source: %d", response.Code)
	}
}

// TestPublishedRegistrationUsesCanonicalStorage exercises publication, rotation and immutable identity against real PostgreSQL.
func TestPublishedRegistrationUsesCanonicalStorage(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	// Integration fixtures require an explicitly configured disposable database.
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := db.InitEnginePostgres(t.Context(), dsn)
	publicationRequire(t, err)
	t.Cleanup(pool.Close)
	catalog := NewCatalogStore(pool)
	id := uuid.New()
	app := securityApp(fusedobject.TokenEndpointAuthMethodClientSecretPost, "")
	app.Auth.Name = "OAuth"
	app.Auth.OAuth2Flows = fusedobject.OAuth2Flows{"authorizationCode": app.Flow}
	key := []byte("01234567890123456789012345678901")
	bucket, version := testoauth.Seed(t, pool, id, app.Auth, key, app.ClientID, app.ClientSecret)
	registration := Registration{BucketID: bucket, ServiceVersionID: version, FlowName: "authorizationCode"}
	// Existing bucket credentials are private until the exact registration is published.
	if _, err := catalog.GetProviderApp(t.Context(), id, "OAuth", key); err == nil {
		t.Fatal("unpublished bucket exposed")
	}
	publicationRequire(t, catalog.PublishRegistration(t.Context(), id, "OAuth", registration, key))
	publicationRequire(t, catalog.PublishRegistration(t.Context(), id, "OAuth", registration, key))
	stored, err := catalog.GetProviderApp(t.Context(), id, "OAuth", key)
	publicationRequire(t, err)
	if !reflect.DeepEqual(stored, app) {
		t.Fatal("canonical contract or bucket pair changed")
	}
	runtime := store.NewPostgresStore(pool)
	testoauth.SavePair(t, runtime, bucket, id, "OAuth", key, app.ClientID, "rotated-secret")
	stored, err = catalog.GetProviderApp(t.Context(), id, "OAuth", key)
	publicationRequire(t, err)
	// Secret rotation should take effect without copying credentials into another table.
	if stored.ClientSecret != "rotated-secret" {
		t.Fatal("canonical secret rotation not observed")
	}
	testoauth.SavePair(t, runtime, bucket, id, "OAuth", key, "different-client", "rotated-secret")
	// A different provider client cannot silently take over an existing publication.
	if _, err := catalog.GetProviderApp(t.Context(), id, "OAuth", key); err == nil {
		t.Fatal("client identity changed silently")
	}
	if err := catalog.PublishRegistration(t.Context(), id, "OAuth", registration, key); err == nil {
		t.Fatal("publication rebound to another client")
	}
	// Restore the original client and prove edits to the pinned contract also fail closed.
	testoauth.SavePair(t, runtime, bucket, id, "OAuth", key, app.ClientID, "rotated-secret")
	_, err = pool.Exec(t.Context(), `UPDATE fused_service_contract_snapshots SET service_metadata=jsonb_set(service_metadata,'{auth_configs,0,oauth2_flows,authorizationCode,token_url}','"https://changed.example/token"'::jsonb) WHERE service_version_id=$1`, version)
	publicationRequire(t, err)
	assertPublicationUnavailable(t, catalog, id, registration, key)
	// Bucket deletion must remain possible without freeing this publication identity for reassignment.
	_, err = pool.Exec(t.Context(), `DELETE FROM fused_buckets WHERE id=$1`, bucket)
	publicationRequire(t, err)
	assertPublicationUnavailable(t, catalog, id, registration, key)

}

// publicationRequire makes setup and SQL failures visible before any security assertion.
func publicationRequire(t *testing.T, err error) {
	t.Helper()
	// A failed setup must not count as a passed denial test.
	if err != nil {
		t.Fatal(err)
	}
}

// TestTokenPolicyHTTPBoundary proves malicious JSON traversing the production handler cannot select the secret destination.
func TestTokenPolicyHTTPBoundary(t *testing.T) {
	for _, action := range []string{"exchange", "refresh"} {
		calls := 0
		service := securityService(t, securityApp(fusedobject.TokenEndpointAuthMethodClientSecretPost, ""), func(r *http.Request) (*http.Response, error) {
			calls++
			// Inspect the actual HTTP transport destination after production request decoding.
			if r.URL.String() != "https://provider.example/token" {
				t.Fatal("caller redirected provider credentials")
			}
			return securityResponse(r, 200, ""), nil
		})
		router := chi.NewRouter()
		router.Post("/{serviceID}/{authName}/exchange", exchangeHandler(service))
		router.Post("/{serviceID}/{authName}/refresh", connectRefreshHandler(service))
		payload, _ := json.Marshal(map[string]any{"code": "fixture-code", "refresh_token": "fixture-refresh", "redirect_uri": "https://consumer.example/callback", "auth": map[string]any{"type": "oauth2", "token_endpoint_auth_method": "client_secret_post"}, "flow": map[string]string{"token_url": "https://attacker.example/collect"}})
		req := httptest.NewRequest(http.MethodPost, "/"+uuid.NewString()+"/OAuth/"+action, strings.NewReader(string(payload)))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		// Policy enforcement belongs to the service, independent of installation authentication middleware.
		if response.Code != 200 || calls != 1 {
			t.Fatalf("%s status=%d calls=%d", action, response.Code, calls)
		}
	}
}

// assertPublicationUnavailable rejects both use and silent republication after source material has drifted or disappeared.
func assertPublicationUnavailable(t *testing.T, catalog *CatalogStore, id uuid.UUID, registration Registration, key []byte) {
	t.Helper()
	// No missing source can inherit metadata or credentials from the consumer.
	if _, err := catalog.GetProviderApp(t.Context(), id, "OAuth", key); err == nil {
		t.Fatal("changed publication usable")
	}
	if err := catalog.PublishRegistration(t.Context(), id, "OAuth", registration, key); err == nil {
		t.Fatal("changed publication overwritten")
	}
}
