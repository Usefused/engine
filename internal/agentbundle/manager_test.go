package agentbundle

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLocalTLSPinsChild exercises the runtime boundary with local fixtures so failures cannot rely on external accounts.
func TestLocalTLSPinsChild(t *testing.T) {
	dir := t.TempDir()
	transport, err := localTLS(dir)
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "local.crt"), filepath.Join(dir, "local.key"))
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	trusted := httptest.NewUnstartedServer(handler)
	trusted.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	trusted.StartTLS()
	defer trusted.Close()
	client := &http.Client{Transport: transport}
	response, err := client.Get(trusted.URL)
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	wrong := httptest.NewTLSServer(handler)
	defer wrong.Close()
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if response, err = client.Get(wrong.URL); err == nil {
		response.Body.Close()
		t.Fatal("trusted another local listener")
	}
}

// TestManagerFailureDoesNotExposeEndpoint exercises the runtime boundary with local fixtures so failures cannot rely on external accounts.
func TestManagerFailureDoesNotExposeEndpoint(t *testing.T) {
	manager, err := New(Options{EngineURL: "http://127.0.0.1:1234", CacheDir: t.TempDir(), ArchivePath: filepath.Join(t.TempDir(), "missing")})
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err != nil {
		t.Fatal(err)
	}
	manager.Start(context.Background())
	defer manager.Close()
	select {
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	case <-manager.done:
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	case <-time.After(3 * time.Second):
		t.Fatal("manager did not report failure")
	}
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if manager.Status().State != "unavailable" {
		t.Fatal(manager.Status())
	}
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if endpoint, _ := manager.Endpoint(); endpoint != "" {
		t.Fatal(endpoint)
	}
}

// TestChildEnvironmentUsesEngineAuthority exercises the runtime boundary with local fixtures so failures cannot rely on external accounts.
func TestChildEnvironmentUsesEngineAuthority(t *testing.T) {
	t.Setenv("FUSED_ENGINE_URL", "http://wrong")
	t.Setenv("HARNEST_DATABASE_URL", "shared")
	t.Setenv("FUSED_API_KEY", "service")
	env := strings.Join(childEnvironment(Options{EngineURL: "http://127.0.0.1:1234", GatewayURL: "https://registry.example/agent/v1", GatewayKey: "test-license", IdentityKey: "test-identity"}), "\n")
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if strings.Contains(env, "http://wrong") || strings.Contains(env, "HARNEST_DATABASE_URL=") || strings.Contains(env, "FUSED_API_KEY=") {
		t.Fatal("inherited shared authority")
	}
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if !strings.Contains(env, "FUSED_AGENT_GATEWAY_URL=https://registry.example/agent/v1") {
		t.Fatal("missing configured model gateway")
	}
}

// Opt-in real portable runtime smoke test; does not call a model or require a service key.
func TestPortableRuntimeLifecycle(t *testing.T) {
	archive := os.Getenv("FUSED_TEST_AGENT_ARCHIVE")
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if archive == "" {
		t.Skip("portable runtime archive not supplied")
	}
	manager, err := New(Options{EngineURL: "http://127.0.0.1:1234", CacheDir: t.TempDir(), ArchivePath: archive, GatewayURL: "https://registry.invalid/agent/v1", GatewayKey: "test-license", IdentityKey: "test-identity", Model: "fused-agent"})
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err != nil {
		t.Fatal(err)
	}
	manager.Start(context.Background())
	defer manager.Close()
	deadline := time.After(2 * time.Minute)
	for {
		state := manager.Status()
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		if state.State == "ready" {
			break
		}
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		if state.State == "unavailable" {
			t.Fatal(state.Message)
		}
		select {
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		case <-deadline:
			t.Fatal("agent did not become ready")
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		case <-time.After(200 * time.Millisecond):
		}
	}
	endpoint, transport := manager.Endpoint()
	response, err := (&http.Client{Transport: transport}).Get(endpoint + "/agent")
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if manager.Status().State == "ready" {
		t.Fatal("agent still ready after shutdown")
	}
}
