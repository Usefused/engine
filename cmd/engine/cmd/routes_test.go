package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/api"
	"github.com/Usefused/engine/internal/engine/store"
)

// stubStore satisfies store.Store for wiring tests. Only the API-key lookup
// registerProxyRoutes' handlers actually call is given real behavior.
type stubStore struct {
	store.Store
	accountID uuid.UUID
}

func (s *stubStore) GetAccountByAPIKey(ctx context.Context, apiKey string) (uuid.UUID, error) {
	return s.accountID, nil
}

// newTestEngineAndRegistry boots a real chi router wired via
// registerProxyRoutes in front of a mock Registry HTTP server, and returns
// both plus a way to inspect what the "Registry" last received. This is the
// integration test S1-B4 calls for: verify the Engine forwards to the
// Registry with correct headers, exercising real chi routing end to end
// rather than mocking Forwarder like the unit tests in internal/engine/api do.
func newTestEngineAndRegistry(t *testing.T) (engineURL string, gotPath, gotAPIKey *string) {
	t.Helper()
	gotPath = new(string)
	gotAPIKey = new(string)

	registryMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		*gotAPIKey = r.Header.Get("X-API-Key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(registryMock.Close)

	proxy := api.NewRegistryProxy(registryMock.URL)
	s := &stubStore{accountID: uuid.New()}

	r := chi.NewRouter()
	registerProxyRoutes(r, proxy, s)

	engine := httptest.NewServer(r)
	t.Cleanup(engine.Close)

	return engine.URL, gotPath, gotAPIKey
}

func TestRegisterProxyRoutes_GraphQLForwardsToRegistry(t *testing.T) {
	engineURL, gotPath, gotAPIKey := newTestEngineAndRegistry(t)

	req, _ := http.NewRequest(http.MethodPost, engineURL+"/graphql", bytes.NewBufferString(`{"query":"{ services { id } }"}`))
	req.Header.Set("X-API-Key", "fsk_test_key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request to Engine failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from Engine, got %d", resp.StatusCode)
	}
	if *gotPath != "/graphql" {
		t.Errorf("expected Registry to receive /graphql, got %q", *gotPath)
	}
	if *gotAPIKey != "fsk_test_key" {
		t.Errorf("expected Registry to receive the original API key unchanged, got %q", *gotAPIKey)
	}
}

// TestRegisterProxyRoutes_RESTPathsForwardToRegistry exercises every path in
// api.RESTProxyMountPaths end to end, verifying chi's Mount preserves the
// full request path (no stripping) so the Registry sees the same path the
// browser sent -- the assumption every RESTProxyHandler call in rest_proxy.go
// depends on (stripPrefix is always "").
func TestRegisterProxyRoutes_RESTPathsForwardToRegistry(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		expectPath string
	}{
		{"integrations tree", http.MethodGet, "/integrations/svc-123", "/integrations/svc-123"},
		{"account tree", http.MethodGet, "/account", "/account"},
		{"sdks tree", http.MethodGet, "/sdks/abc", "/sdks/abc"},
		{"leads single endpoint", http.MethodPost, "/leads", "/leads"},
		{"credits tree", http.MethodGet, "/credits/pricing", "/credits/pricing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engineURL, gotPath, gotAPIKey := newTestEngineAndRegistry(t)

			req, _ := http.NewRequest(tt.method, engineURL+tt.path, nil)
			req.Header.Set("X-API-Key", "fsk_test_key")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request to Engine failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected 200 from Engine, got %d", resp.StatusCode)
			}
			if *gotPath != tt.expectPath {
				t.Errorf("expected Registry to receive %q, got %q", tt.expectPath, *gotPath)
			}
			if *gotAPIKey != "fsk_test_key" {
				t.Errorf("expected Registry to receive the original API key, got %q", *gotAPIKey)
			}
		})
	}
}

func TestRegisterProxyRoutes_WorkspaceNotMounted(t *testing.T) {
	engineURL, _, _ := newTestEngineAndRegistry(t)

	req, _ := http.NewRequest(http.MethodGet, engineURL+"/workspace/services", nil)
	req.Header.Set("X-API-Key", "fsk_test_key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request to Engine failed: %v", err)
	}
	defer resp.Body.Close()

	// registerProxyRoutes doesn't mount /workspace at all (Sprint 2 owns
	// that, pointed at the Engine's own store, not this Registry proxy) --
	// a bare chi.NewRouter() with nothing else registered 404s on it.
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for /workspace (not proxied), got %d", resp.StatusCode)
	}
}
