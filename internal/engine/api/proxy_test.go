package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testRegistryLicense = "fsk_registry_license"

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

	proxy := NewRegistryProxy(backend.URL, testRegistryLicense)

	req := httptest.NewRequest(http.MethodGet, "/graphql", nil)
	req.Host = "engine.internal:8081"
	rec := httptest.NewRecorder()

	proxy.Forward(rec, req, "")

	backendHost := backend.URL[len("http://"):]
	if gotHost != backendHost {
		t.Errorf("expected backend to see Host %q, got %q", backendHost, gotHost)
	}
}

// TestForward_InjectsLicenseIdentity verifies local credentials stop at the
// Engine and only the licensed workspace identity reaches Registry.
func TestForward_InjectsLicenseIdentity(t *testing.T) {
	var gotKey, gotAuthorization string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	proxy := NewRegistryProxy(backend.URL, testRegistryLicense)

	req := httptest.NewRequest(http.MethodGet, "/integrations/svc-123", nil)
	req.Header.Set("X-API-Key", "fsk_local_personal_key")
	req.Header.Set("Authorization", "Bearer local-personal-key")
	rec := httptest.NewRecorder()

	proxy.Forward(rec, req, "")

	if gotKey != testRegistryLicense || gotAuthorization != "Bearer "+testRegistryLicense {
		t.Errorf("Registry auth = X-API-Key %q, Authorization %q", gotKey, gotAuthorization)
	}
}

func TestForward_StripsSensitiveRegistryResponseHeaders(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Authorization", "Bearer must-not-leak")
		w.Header().Set("Proxy-Authorization", "must-not-leak")
		w.Header().Set("X-API-Key", "must-not-leak")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	proxy := NewRegistryProxy(backend.URL, testRegistryLicense)
	recorder := httptest.NewRecorder()
	proxy.Forward(recorder, httptest.NewRequest(http.MethodGet, "/account", nil), "")
	for _, header := range []string{"Authorization", "Proxy-Authorization", "X-API-Key"} {
		if got := recorder.Header().Get(header); got != "" {
			t.Fatalf("response header %s leaked %q", header, got)
		}
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

			proxy := NewRegistryProxy(backend.URL, testRegistryLicense)

			req := httptest.NewRequest(http.MethodPost, "/graphql", nil)
			rec := httptest.NewRecorder()

			proxy.Forward(rec, req, "")

			if rec.Code != tt.status {
				t.Errorf("expected status %d, got %d", tt.status, rec.Code)
			}
		})
	}
}

// TestForwardAndInspect_PassesBodyThroughUnchangedAndInvokesOnSuccess proves the
// client and pre-write callback receive the same Registry response bytes.
func TestForwardAndInspect_PassesBodyThroughUnchangedAndInvokesOnSuccess(t *testing.T) {
	wantBody := `{"status":"applied","service_id":"abc-123","version":"2026-01-01"}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wantBody))
	}))
	defer backend.Close()

	proxy := NewRegistryProxy(backend.URL, testRegistryLicense)
	req := httptest.NewRequest(http.MethodPost, "/integrations/import/apply", nil)
	rec := httptest.NewRecorder()

	var gotOnSuccessBody []byte
	onSuccessCalls := 0
	proxy.ForwardAndInspect(rec, req, "", func(_ *http.Response, body []byte) {
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

// TestForwardAndInspect_AllowsStructuredResponseReplacement verifies local follow-up errors win before proxy output.
func TestForwardAndInspect_AllowsStructuredResponseReplacement(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"applied"}`))
	}))
	defer backend.Close()

	proxy := NewRegistryProxy(backend.URL, testRegistryLicense)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/integrations/import/apply", nil)
	proxy.ForwardAndInspect(recorder, request, "", func(response *http.Response, _ []byte) {
		replaceProxyJSONResponse(response, http.StatusFailedDependency, []byte(`{"error":{"code":"partial"}}`))
	})

	if recorder.Code != http.StatusFailedDependency || recorder.Body.String() != `{"error":{"code":"partial"}}` {
		t.Fatalf("replacement response = status %d body %q", recorder.Code, recorder.Body.String())
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

	proxy := NewRegistryProxy(backend.URL, testRegistryLicense)
	req := httptest.NewRequest(http.MethodPost, "/integrations/import/apply", nil)
	rec := httptest.NewRecorder()

	onSuccessCalls := 0
	proxy.ForwardAndInspect(rec, req, "", func(_ *http.Response, body []byte) {
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

// TestBillingOriginComesFromEngineConfig proves browser headers cannot select a billing return destination.
func TestBillingOriginComesFromEngineConfig(t *testing.T) {
	for _, tc := range []struct{ method, path, configured, want string }{
		{http.MethodPost, "/account/billing/link", "http://127.0.0.1:53182", "http://127.0.0.1:53182"},
		{http.MethodPost, "/account/billing/link", "", ""},
		{http.MethodGet, "/account/billing", "https://engine.example", ""},
		{http.MethodPost, "/integrations/import/plan", "https://engine.example", ""},
	} {
		var got string
		// Capture only the effective header received by Registry after the real reverse proxy boundary.
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get("X-Fused-Billing-Engine-Origin")
			w.WriteHeader(http.StatusOK)
		}))
		proxy := NewRegistryProxy(backend.URL, testRegistryLicense, tc.configured)
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Fused-Billing-Engine-Origin", "https://attacker.example")
		req.Header.Set("Origin", "https://attacker.example")
		req.Host = "attacker.example"
		proxy.Forward(httptest.NewRecorder(), req, "")
		backend.Close()
		// The configured value is authoritative only on the intended billing mutation route.
		if got != tc.want {
			t.Fatalf("%s %s: got origin %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

// TestBillingSyncRefreshesAccessBeforeSuccess prevents paid upgrades waiting for a periodic heartbeat or reporting failed persistence as success.
func TestBillingSyncRefreshesAccessBeforeSuccess(t *testing.T) {
	for _, tc := range []struct {
		path          string
		upstream      int
		refreshed     bool
		calls, status int
	}{
		{"/account/billing/sync", 200, true, 1, 200}, {"/account/billing/sync", 200, false, 1, 503},
		{"/account/billing/sync", 503, true, 0, 503}, {"/account/billing/link", 200, true, 0, 200},
	} {
		// Model Registry reconciliation independently from local entitlement persistence.
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.upstream) }))
		proxy := NewRegistryProxy(backend.URL, testRegistryLicense)
		calls := 0
		proxy.BillingSync = func(context.Context) bool { calls++; return tc.refreshed } // Record completion before the response reaches the caller.
		response := httptest.NewRecorder()
		proxy.Forward(response, httptest.NewRequest(http.MethodPost, tc.path, nil), "")
		backend.Close()
		// Link creation and failed synchronization never trigger or claim updated access.
		if calls != tc.calls || response.Code != tc.status {
			t.Fatalf("%+v: calls=%d status=%d", tc, calls, response.Code)
		}
	}
}
