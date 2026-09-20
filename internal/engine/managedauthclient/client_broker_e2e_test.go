package managedauthclient

import (
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
	"github.com/stretchr/testify/require"

	"github.com/Usefused/engine/internal/engine/managedauthbroker"
	"github.com/Usefused/engine/internal/shared/db"
)

// TestManagedAuthClientAndBrokerE2E drives the real client-side Service
// (Store, HTTPBrokerClient, status/enable HTTP handlers) against a REAL
// managedauthbroker server -- both packages live in this module, so unlike
// the Registry ticket hop this is not stood in: it is the actual broker
// code from managedauthbroker, mounted on its own httptest.Server with its
// own real Postgres-backed store, exactly as it runs in production. Only
// Registry's ticket mint is faked, for the reason given on
// fixtureTicketMinter.
func TestManagedAuthClientAndBrokerE2E(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := db.InitEnginePostgres(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("initialize Engine database: %v", err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		// t.Context() is already canceled by the time Cleanup runs, so this
		// deliberately uses a fresh background context for the DELETE.
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_managed_auth_broker_credential`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_managed_auth_installations`)
	})

	// -- Real broker server, real store, real Postgres.
	installs := managedauthbroker.NewStore(pool)
	remoteAccountID := uuid.New()
	remoteInstallationID := uuid.New()
	registryFixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"account_id": remoteAccountID.String(), "installation_id": remoteInstallationID.String(), "expires_at": time.Now().Add(5 * time.Minute).Format(time.RFC3339Nano)})
	}))
	t.Cleanup(registryFixture.Close)
	verifier := managedauthbroker.NewRegistryTicketVerifier(registryFixture.URL+"/graphql", nil)
	brokerService, err := managedauthbroker.NewService(installs, verifier)
	if err != nil {
		t.Fatalf("NewService (broker): %v", err)
	}
	brokerRouter := chi.NewRouter()
	managedauthbroker.MountRoutes(brokerRouter, brokerService)
	brokerServer := httptest.NewServer(brokerRouter)
	t.Cleanup(brokerServer.Close)

	// -- Real client-side Service, real store, real Postgres, real HTTP
	// broker client pointed at the real broker server above.
	clientStore := NewStore(pool)
	masterKey := make([]byte, 32)
	if _, err := rand.Read(masterKey); err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	minter := &countingTicketMinter{ticket: "any-ticket-the-fixture-accepts"}
	broker := NewHTTPBrokerClient(brokerServer.URL, nil)
	service, err := NewService(clientStore, minter, broker, masterKey)
	if err != nil {
		t.Fatalf("NewService (client): %v", err)
	}

	router := chi.NewRouter()
	MountRoutes(router, service)
	clientServer := httptest.NewServer(router)
	t.Cleanup(clientServer.Close)

	getStatus := func(t *testing.T) string {
		t.Helper()
		resp, err := http.Get(clientServer.URL + "/workspace/managed-auth")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		var out managedAuthStatusResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status: expected 200, got %d", resp.StatusCode)
		}
		return out.Status
	}

	t.Run("status before enrollment is enrollment_required", func(t *testing.T) {
		if status := getStatus(t); status != string(StatusEnrollmentRequired) {
			t.Fatalf("expected %q, got %q", StatusEnrollmentRequired, status)
		}
	})

	t.Run("PUT enables enrollment end to end against the real broker", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, clientServer.URL+"/workspace/managed-auth", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("enable: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("enable: expected 200, got %d", resp.StatusCode)
		}
		if status := getStatus(t); status != string(StatusReady) {
			t.Fatalf("expected %q after enable, got %q", StatusReady, status)
		}
	})

	t.Run("AccessToken returns the credential the real broker issued", func(t *testing.T) {
		token, err := service.AccessToken(t.Context())
		if err != nil {
			t.Fatalf("AccessToken: %v", err)
		}
		if token == "" {
			t.Fatal("expected a non-empty access token")
		}
	})

	t.Run("EnsureFresh is a no-op well before expiry", func(t *testing.T) {
		before, err := clientStore.Get(t.Context(), masterKey)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := service.EnsureFresh(t.Context()); err != nil {
			t.Fatalf("EnsureFresh: %v", err)
		}
		after, err := clientStore.Get(t.Context(), masterKey)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if before.AccessToken != after.AccessToken {
			t.Fatal("EnsureFresh refreshed a token that was not close to expiry")
		}
	})

	t.Run("a near-expiry access token is refreshed against the real broker, rotating the refresh token too", func(t *testing.T) {
		before, err := clientStore.Get(t.Context(), masterKey)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		// Force the stored access token to look like it is about to expire,
		// the same way real elapsed time would, without waiting an hour.
		if err := clientStore.Save(t.Context(), before.AccessToken, before.RefreshToken, time.Now().Add(time.Minute), before.RefreshExpiresAt, masterKey); err != nil {
			t.Fatalf("force near-expiry: %v", err)
		}
		if err := service.EnsureFresh(t.Context()); err != nil {
			t.Fatalf("EnsureFresh: %v", err)
		}
		after, err := clientStore.Get(t.Context(), masterKey)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if after.AccessToken == before.AccessToken {
			t.Fatal("expected a rotated access token from the real broker refresh")
		}
		if !after.AccessExpiresAt.After(time.Now().Add(3 * time.Minute)) {
			t.Fatalf("expected the refreshed access token to have a fresh expiry, got %v", after.AccessExpiresAt)
		}

		// The broker's own refresh-token reuse detection must now reject the
		// prior (superseded) refresh token if presented again.
		if _, _, _, err := broker.Refresh(t.Context(), before.RefreshToken, "fresh-ticket"); err == nil {
			t.Fatal("expected the broker to reject the superseded refresh token")
		}
	})
}

// countingTicketMinter is a trivial TicketMinter: the registryFixture above
// accepts any non-empty ticket, so this only needs to hand back a fixed
// value, but it is a distinct type (rather than reusing a broker test
// fixture) to keep this test's dependency on Registry's contract explicit
// and self-contained.
type countingTicketMinter struct{ ticket string }

func (m *countingTicketMinter) MintManagedAuthEnrollmentTicket(context.Context) (string, error) {
	return m.ticket, nil
}
