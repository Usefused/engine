package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestForward_RewritesHost verifies that the Host header seen by the Registry
// is the Registry's own host, not whatever Host the browser sent to the Engine.
// Without this, a naive reverse proxy would forward the Engine's Host header,
// which can break Registry-side routing/vhost logic that trusts Host.
func TestForward_RewritesHost(t *testing.T) {
	var gotHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	proxy := NewRegistryProxy(backend.URL)

	req := httptest.NewRequest(http.MethodGet, "/graphql", nil)
	req.Host = "engine.internal:8081"
	rec := httptest.NewRecorder()

	proxy.Forward(rec, req, "")

	backendHost := backend.URL[len("http://"):]
	if gotHost != backendHost {
		t.Errorf("expected backend to see Host %q, got %q", backendHost, gotHost)
	}
}

// TestForward_PreservesAPIKey verifies X-API-Key is forwarded byte-for-byte.
// The Registry owns identity resolution for its own endpoints; the Engine
// must not mutate, strip, or re-derive the key before relaying it.
func TestForward_PreservesAPIKey(t *testing.T) {
	var gotKey string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	proxy := NewRegistryProxy(backend.URL)

	req := httptest.NewRequest(http.MethodGet, "/integrations/svc-123", nil)
	req.Header.Set("X-API-Key", "fsk_original_unmodified_key")
	rec := httptest.NewRecorder()

	proxy.Forward(rec, req, "")

	if gotKey != "fsk_original_unmodified_key" {
		t.Errorf("expected X-API-Key to pass through unchanged, got %q", gotKey)
	}
}

// TestForward_PropagatesStatusCode verifies 4xx/5xx from the Registry reach
// the browser unchanged -- the Engine is a transparent relay, not a place
// that should mask or translate Registry error codes.
func TestForward_PropagatesStatusCode(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"NotFound", http.StatusNotFound},
		{"ServerError", http.StatusInternalServerError},
		{"BadRequest", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer backend.Close()

			proxy := NewRegistryProxy(backend.URL)

			req := httptest.NewRequest(http.MethodPost, "/graphql", nil)
			rec := httptest.NewRecorder()

			proxy.Forward(rec, req, "")

			if rec.Code != tt.status {
				t.Errorf("expected status %d, got %d", tt.status, rec.Code)
			}
		})
	}
}

// TestForwardAndInspect_PassesBodyThroughUnchangedAndInvokesOnSuccess is
// Task 3's core contract (engine_workspace_registration_plan.md): the client
// must receive the exact same bytes Forward would have given it, and
// onSuccess must see those same bytes -- the import/apply auto-register
// intercept reads from onSuccess's copy, never from what's written to w.
func TestForwardAndInspect_PassesBodyThroughUnchangedAndInvokesOnSuccess(t *testing.T) {
	wantBody := `{"status":"applied","service_id":"abc-123","is_new_service":true,"version":"2026-01-01"}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wantBody))
	}))
	defer backend.Close()

	proxy := NewRegistryProxy(backend.URL)
	req := httptest.NewRequest(http.MethodPost, "/integrations/import/apply", nil)
	rec := httptest.NewRecorder()

	var gotOnSuccessBody []byte
	onSuccessCalls := 0
	proxy.ForwardAndInspect(rec, req, "", func(body []byte) {
		onSuccessCalls++
		gotOnSuccessBody = body
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	clientBody, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read client body: %v", err)
	}
	if string(clientBody) != wantBody {
		t.Errorf("expected client to receive %q, got %q", wantBody, string(clientBody))
	}
	if onSuccessCalls != 1 {
		t.Fatalf("expected onSuccess called exactly once, got %d", onSuccessCalls)
	}
	if string(gotOnSuccessBody) != wantBody {
		t.Errorf("expected onSuccess to see %q, got %q", wantBody, string(gotOnSuccessBody))
	}
}

// TestForwardAndInspect_SkipsOnSuccessForNonSuccessStatus asserts a failed
// apply never triggers auto-registration -- there's nothing to register from
// an error response -- while the client still sees the real error body.
func TestForwardAndInspect_SkipsOnSuccessForNonSuccessStatus(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"source hash mismatch"}`))
	}))
	defer backend.Close()

	proxy := NewRegistryProxy(backend.URL)
	req := httptest.NewRequest(http.MethodPost, "/integrations/import/apply", nil)
	rec := httptest.NewRecorder()

	onSuccessCalls := 0
	proxy.ForwardAndInspect(rec, req, "", func(body []byte) {
		onSuccessCalls++
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if onSuccessCalls != 0 {
		t.Errorf("expected onSuccess never called for a non-2xx response, got %d calls", onSuccessCalls)
	}
	clientBody, _ := io.ReadAll(rec.Body)
	if string(clientBody) != `{"error":"source hash mismatch"}` {
		t.Errorf("expected error body to still reach the client unchanged, got %q", string(clientBody))
	}
}
