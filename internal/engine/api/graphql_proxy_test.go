package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Usefused/engine/internal/engine/store"
)

// mockKeyStore implements store.Store for handler tests, overriding only the
// API-key lookup GraphQLProxyHandler/RESTProxyHandler actually call. Embedding
// store.Store means adding new methods to the interface later won't break
// this mock -- only the methods under test need real behavior.
type mockKeyStore struct {
	store.Store
	accountID uuid.UUID
	err       error
}

func (m *mockKeyStore) GetAccountByAPIKey(ctx context.Context, apiKey string) (uuid.UUID, error) {
	return m.accountID, m.err
}

// mockForwarder records whether Forward was invoked, without making a real
// network call. graphql_proxy.go and rest_proxy.go depend on the Forwarder
// interface rather than the concrete *RegistryProxy specifically so tests
// can substitute this instead of standing up a real Registry.
type mockForwarder struct {
	called bool
}

func (m *mockForwarder) Forward(w http.ResponseWriter, r *http.Request, stripPrefix string) {
	m.called = true
	w.WriteHeader(http.StatusOK)
}

func (m *mockForwarder) ForwardAndInspect(w http.ResponseWriter, r *http.Request, stripPrefix string, onSuccess func(body []byte)) {
	m.called = true
	w.WriteHeader(http.StatusOK)
	if onSuccess != nil {
		onSuccess(nil)
	}
}

// setupTestTracer installs an in-memory span exporter as the global tracer
// provider for the duration of a test, then restores whatever was installed
// before -- handler code under test calls otel.Tracer("engine") directly
// (matching the rest of the Engine, e.g. dispatcher.go), so intercepting
// spans means swapping the global provider, not injecting a dependency.
func setupTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return exporter
}

func TestGraphQLProxy_ValidKey_Forwards(t *testing.T) {
	s := &mockKeyStore{accountID: uuid.New()}
	fwd := &mockForwarder{}
	handler := GraphQLProxyHandler(fwd, s)

	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewBufferString(`{"query":"{ services { id } }"}`))
	req.Header.Set("X-API-Key", "fsk_valid")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !fwd.called {
		t.Error("expected Forward to be called for a valid key")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestGraphQLProxy_InvalidKey_Returns401(t *testing.T) {
	s := &mockKeyStore{err: errors.New("api key not found")}
	fwd := &mockForwarder{}
	handler := GraphQLProxyHandler(fwd, s)

	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewBufferString(`{"query":"{ services { id } }"}`))
	req.Header.Set("X-API-Key", "fsk_invalid")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if fwd.called {
		t.Error("Forward must not be called for an invalid key -- traffic should never reach the Registry")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestGraphQLProxy_MutationEmitsOTELSpan(t *testing.T) {
	exporter := setupTestTracer(t)
	s := &mockKeyStore{accountID: uuid.New()}
	fwd := &mockForwarder{}
	handler := GraphQLProxyHandler(fwd, s)

	payload, _ := json.Marshal(map[string]string{"query": `mutation { activateService(id: "x") { id } }`})
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(payload))
	req.Header.Set("X-API-Key", "fsk_valid")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span for a mutation, got %d", len(spans))
	}
	if spans[0].Name != "engine.proxy.graphql_mutation" {
		t.Errorf("unexpected span name: %s", spans[0].Name)
	}
	if !fwd.called {
		t.Error("expected Forward to still be called for a mutation")
	}
}

func TestGraphQLProxy_QueryAddsServerTiming(t *testing.T) {
	s := &mockKeyStore{accountID: uuid.New()}
	fwd := &mockForwarder{}
	handler := GraphQLProxyHandler(fwd, s)

	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewBufferString(`{"query":"{ sdks { total } }"}`))
	req.Header.Set("X-API-Key", "fsk_valid")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	timing := rec.Header().Get("Server-Timing")
	for _, want := range []string{"engine_auth;dur=", "engine_cache;dur=", "registry;dur=", "engine_total;dur="} {
		if !strings.Contains(timing, want) {
			t.Fatalf("Server-Timing = %q, want metric %q", timing, want)
		}
	}
}

func TestGraphQLProxy_CacheHitAddsServerTiming(t *testing.T) {
	s := &mockKeyStore{accountID: uuid.New()}
	fwd := &mockForwarder{}
	handler := GraphQLProxyHandler(fwd, s)
	body := `{"query":"{ sdks { total } }"}`

	first := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewBufferString(body))
	req.Header.Set("X-API-Key", "fsk_valid")
	handler.ServeHTTP(first, req)

	second := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewBufferString(body))
	req2.Header.Set("X-API-Key", "fsk_valid")
	handler.ServeHTTP(second, req2)

	if second.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT", second.Header().Get("X-Cache"))
	}
	timing := second.Header().Get("Server-Timing")
	if !strings.Contains(timing, `engine_cache_hit;desc="hit"`) {
		t.Fatalf("Server-Timing = %q, want cache hit marker", timing)
	}
}
