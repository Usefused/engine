package managedauthbroker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/testoauth"
)

// TestManagedAuthBrokerE2E drives the real broker HTTP handlers (MountRoutes,
// MountConnectRoutes, MountAdminRoutes), the real Store/CatalogStore/Service/
// ConnectService, and a real PostgreSQL database end to end. Registry's own
// ticket-issuing side is exercised for real in
// backend/internal/registry/api/managed_auth_enrollment_e2e_test.go (a
// separate Go module this one cannot import); here it is stood in by a
// minimal fixture server that implements only the introspection contract
// RegistryTicketVerifier speaks, so this test's focus stays on the broker's
// own enrollment, rotation, and connect-proxy behavior. The third-party
// provider is similarly stood in, since a real e2e test cannot depend on a
// live external OAuth provider; connectauth.ExchangeAuthorizationCode/
// RefreshAccessToken themselves are exercised for real, unmocked.
func TestManagedAuthBrokerE2E(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := db.InitEnginePostgres(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("initialize Engine database: %v", err)
	}
	t.Cleanup(pool.Close)

	serviceID := uuid.New()
	const authName = "e2eOAuth"
	t.Cleanup(func() {
		// t.Context() is already canceled by the time Cleanup runs, so this
		// deliberately uses a fresh background context for the DELETE.
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_oauth_publications WHERE service_id = $1`, serviceID)
	})

	// -- Fixture: a stand-in Registry that only speaks the introspection
	// contract RegistryTicketVerifier calls. "valid-ticket" resolves to a
	// fixed remote account and is single-use, mirroring the real endpoint's
	// contract (tested for real against Registry's own code elsewhere).
	remoteAccountID := uuid.New()
	remoteInstallationID := uuid.New()
	issuableTickets := map[string]bool{"valid-ticket": true, "valid-ticket-2": true, "refresh-ticket": true}
	consumedTickets := map[string]bool{}
	registryFixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Ticket string `json:"ticket"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !issuableTickets[body.Ticket] || consumedTickets[body.Ticket] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		consumedTickets[body.Ticket] = true
		_ = json.NewEncoder(w).Encode(map[string]string{"account_id": remoteAccountID.String(), "installation_id": remoteInstallationID.String(), "expires_at": time.Now().Add(5 * time.Minute).Format(time.RFC3339Nano)})
	}))
	t.Cleanup(registryFixture.Close)

	// -- Fixture: a stand-in third-party OAuth provider token endpoint. It
	// validates the exact client_secret_post form contract
	// connectauth.executeTokenGrant sends and issues a one-time-use code.
	const providerClientSecret = "e2e-provider-secret"
	providerFixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("client_secret") != providerClientSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			if r.PostForm.Get("code") != "valid-code" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "provider-access-1", "refresh_token": "provider-refresh-1",
				"token_type": "Bearer", "expires_in": 3600,
			})
		case "refresh_token":
			if r.PostForm.Get("refresh_token") != "provider-refresh-1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "provider-access-2", "token_type": "Bearer", "expires_in": 3600,
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(providerFixture.Close)

	masterKey := make([]byte, 32)
	if _, err := rand.Read(masterKey); err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	const adminKey = "e2e-admin-key"

	installs := NewStore(pool)
	verifier := NewRegistryTicketVerifier(registryFixture.URL+"/graphql", nil) // exercises the real GraphQL-suffix-stripping base URL logic
	service, err := NewService(installs, verifier)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	catalog := NewCatalogStore(pool)
	connect, err := NewConnectService(catalog, masterKey, providerFixture.Client())
	if err != nil {
		t.Fatalf("NewConnectService: %v", err)
	}

	router := chi.NewRouter()
	MountRoutes(router, service)
	MountConnectRoutes(router, connect, installs)
	MountAdminRoutes(router, catalog, masterKey, adminKey)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	// Register Fused's managed app for this service via the real admin path.
	// Provider metadata and credentials are seeded through the canonical Engine stores, never the publication API.
	publishedAuth := fusedobject.AuthConfig{Name: authName, Type: "oauth2", TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost, OAuth2Flows: fusedobject.OAuth2Flows{"authorizationCode": {TokenURL: providerFixture.URL}}}
	bucket, version := testoauth.Seed(t, pool, serviceID, publishedAuth, masterKey, "fused-managed-client-id", providerClientSecret)
	registerBody, _ := json.Marshal(Registration{BucketID: bucket, ServiceVersionID: version, FlowName: "authorizationCode"})
	registerReq, _ := http.NewRequest(http.MethodPut, server.URL+"/managed-auth/broker/admin/apps/"+serviceID.String()+"/"+authName, bytes.NewReader(registerBody))
	registerReq.Header.Set("Authorization", "Bearer "+adminKey)
	registerResp, err := http.DefaultClient.Do(registerReq)
	if err != nil {
		t.Fatalf("register provider app: %v", err)
	}
	if registerResp.StatusCode != http.StatusNoContent {
		t.Fatalf("register provider app: expected 204, got %d", registerResp.StatusCode)
	}

	t.Run("admin registration without the admin key is rejected", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, server.URL+"/managed-auth/broker/admin/apps/"+serviceID.String()+"/"+authName, bytes.NewReader(registerBody))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	})

	enroll := func(t *testing.T, ticket string) (*http.Response, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"ticket": ticket})
		resp, err := http.Post(server.URL+"/managed-auth/broker/enroll", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("enroll: %v", err)
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		return resp, out
	}

	t.Run("enrolling with an invalid ticket is rejected", func(t *testing.T) {
		resp, _ := enroll(t, "not-a-real-ticket")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	})

	_, enrollBody := enroll(t, "valid-ticket")
	accessToken, _ := enrollBody["access_token"].(string)
	refreshToken, _ := enrollBody["refresh_token"].(string)
	if accessToken == "" || refreshToken == "" {
		t.Fatalf("enroll did not return both tokens: %#v", enrollBody)
	}

	t.Run("client-id is discoverable with a live access token", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/managed-auth/broker/connect/"+serviceID.String()+"/"+authName+"/client-id", nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("client-id: %v", err)
		}
		var out map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != http.StatusOK || out["client_id"] != "fused-managed-client-id" {
			t.Fatalf("expected 200 with the registered client_id, got %d %#v", resp.StatusCode, out)
		}
	})

	t.Run("client-id without a token is rejected", func(t *testing.T) {
		resp, err := http.Get(server.URL + "/managed-auth/broker/connect/" + serviceID.String() + "/" + authName + "/client-id")
		if err != nil {
			t.Fatalf("client-id: %v", err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	})

	authConfig := fusedobject.AuthConfig{Type: "oauth2", TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost}
	flow := fusedobject.OAuth2FlowContract{TokenURL: providerFixture.URL}

	exchange := func(t *testing.T, code string) (*http.Response, map[string]any) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"redirect_uri": "https://customer.example/callback", "auth": authConfig, "flow": flow, "code": code,
		})
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/managed-auth/broker/connect/"+serviceID.String()+"/"+authName+"/exchange", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("exchange: %v", err)
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		return resp, out
	}

	t.Run("a valid code exchanges for the fixture provider's real token response", func(t *testing.T) {
		resp, out := exchange(t, "valid-code")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d (%#v)", resp.StatusCode, out)
		}
		if out["access_token"] != "provider-access-1" || out["refresh_token"] != "provider-refresh-1" {
			t.Fatalf("unexpected token response: %#v", out)
		}
	})

	t.Run("an invalid code is rejected as a bad gateway", func(t *testing.T) {
		resp, _ := exchange(t, "wrong-code")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("expected 502, got %d", resp.StatusCode)
		}
	})

	t.Run("refresh proxies the real refresh-token call to the provider fixture", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]any{
			"redirect_uri": "https://customer.example/callback", "auth": authConfig, "flow": flow, "refresh_token": "provider-refresh-1",
		})
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/managed-auth/broker/connect/"+serviceID.String()+"/"+authName+"/refresh", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != http.StatusOK || out["access_token"] != "provider-access-2" {
			t.Fatalf("expected 200 with the fixture's rotated token, got %d %#v", resp.StatusCode, out)
		}
	})

	t.Run("rotating the installation credential revokes the prior access token", func(t *testing.T) {
		rotateBody, _ := json.Marshal(map[string]string{"refresh_token": refreshToken, "ticket": "refresh-ticket"})
		resp, err := http.Post(server.URL+"/managed-auth/broker/refresh", "application/json", bytes.NewReader(rotateBody))
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		var rotated map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&rotated)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("rotate: expected 200, got %d", resp.StatusCode)
		}
		newAccessToken, _ := rotated["access_token"].(string)
		if newAccessToken == "" || newAccessToken == accessToken {
			t.Fatalf("rotation did not issue a distinct access token: %#v", rotated)
		}

		// The old access token must stop working immediately.
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/managed-auth/broker/connect/"+serviceID.String()+"/"+authName+"/client-id", nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		staleResp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("stale client-id request: %v", err)
		}
		if staleResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected the rotated-away access token to be rejected, got %d", staleResp.StatusCode)
		}

		// The presented refresh token cannot be reused (rotation is single-use).
		reuseResp, err := http.Post(server.URL+"/managed-auth/broker/refresh", "application/json", bytes.NewReader(rotateBody))
		if err != nil {
			t.Fatalf("reuse rotate: %v", err)
		}
		if reuseResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected reused refresh token to be rejected, got %d", reuseResp.StatusCode)
		}
	})

	t.Run("re-enrolling the same remote account supersedes the still-live installation", func(t *testing.T) {
		_, secondEnroll := enroll(t, "valid-ticket-2")
		secondAccessToken, _ := secondEnroll["access_token"].(string)
		if secondAccessToken == "" {
			t.Fatalf("second enroll did not return an access token: %#v", secondEnroll)
		}
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/managed-auth/broker/connect/"+serviceID.String()+"/"+authName+"/client-id", nil)
		req.Header.Set("Authorization", "Bearer "+secondAccessToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("client-id with re-enrolled token: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for the newly-enrolled installation, got %d", resp.StatusCode)
		}
	})
}
