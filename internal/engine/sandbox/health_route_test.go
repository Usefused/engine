package sandbox_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/go-chi/chi/v5"
)

// TestSandboxInitializationPreservesReadiness rejects transport wiring that hides an unavailable worker.
func TestSandboxInitializationPreservesReadiness(t *testing.T) {
	router := chi.NewRouter()
	// Model the Engine's authoritative isolation failure before transports are initialized.
	router.Get("/health", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"unified_app_worker_ready":false}`))
	})
	sandbox.InitSandbox(router, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, "", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	// A static transport health response must not turn failed Engine readiness into HTTP 200.
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != `{"unified_app_worker_ready":false}` {
		t.Fatalf("readiness was overwritten: %d %s", response.Code, response.Body.String())
	}
}
