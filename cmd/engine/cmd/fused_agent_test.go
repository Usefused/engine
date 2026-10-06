package cmd

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/google/uuid"
)

// TestAgentProxyReplacesCredentials ensures browser authority never reaches model-facing processes.
func TestAgentProxyReplacesCredentials(t *testing.T) {
	actor := accesscontrol.Actor{WorkspaceID: uuid.New(), SubjectID: uuid.New(), Kind: accesscontrol.SubjectUser}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cookies, bootstrap credentials and request paths cannot be borrowed by the child.
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-API-Key") != "" || r.URL.Path != "/sessions" {
			t.Error("browser authority forwarded")
		}
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			t.Error("missing agent identity")
			w.WriteHeader(401)
			return
		} // Require a signed actor token.
		payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		_ = json.Unmarshal(payload, &claims)
		if claims["workspace_id"] != actor.WorkspaceID.String() || claims["aud"] != "fused-agent" {
			t.Error("identity scope missing")
		} // Session scope must survive proxying.
		w.Header().Set("Set-Cookie", "bad=cookie")
		io.WriteString(w, `{"id":"session"}`)
	}))
	defer upstream.Close()
	request := httptest.NewRequest("POST", "/agent/sessions", strings.NewReader(`{}`))
	request.Header.Set("Cookie", "session=private")
	request.Header.Set("X-API-Key", "private")
	request.Header.Set("Authorization", "Bearer browser-private")
	response := httptest.NewRecorder()
	serveFusedAgent(response, request, upstream.URL, nil, "test-only-identity-key")
	if response.Code != 401 {
		t.Fatal("anonymous agent access allowed")
	} // Defence in depth is independent of router middleware.
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response = httptest.NewRecorder()
	serveFusedAgent(response, request, upstream.URL, nil, "test-only-identity-key")
	if response.Code != 200 || response.Header().Get("Set-Cookie") != "" {
		t.Fatalf("unexpected proxy result %d", response.Code)
	} // Child cookies cannot establish browser authority.
}

// TestAgentIdentitySeparatesActorsAndWorkspaces prevents guessed session IDs from crossing ownership boundaries.
func TestAgentIdentitySeparatesActorsAndWorkspaces(t *testing.T) {
	actor := accesscontrol.Actor{WorkspaceID: uuid.New(), SubjectID: uuid.New(), Kind: accesscontrol.SubjectUser}
	now := time.Now()
	first := fusedAgentIdentity(actor, "key", now)
	actor.SubjectID = uuid.New()
	if first == fusedAgentIdentity(actor, "key", now) {
		t.Fatal("actor identities collide")
	} // Subject identity is stable but not shared.
	actor.WorkspaceID = uuid.New()
	if first == fusedAgentIdentity(actor, "key", now) {
		t.Fatal("workspace identities collide")
	} // Workspace ownership is part of identity.
}

// TestAgentDefaultsToRegistry keeps custom model gateways from accidentally receiving the license key.
func TestAgentDefaultsToRegistry(t *testing.T) {
	engine := config.EngineConfig{RegistryEndpoint: "https://registry.example/graphql", LicenseKey: "license-only"}
	options, err := fusedAgentOptions(engine, "identity")
	// Production's GraphQL endpoint must resolve to the licensed REST gateway.
	if err != nil || options.GatewayURL != "https://registry.example/agent/v1" || options.GatewayKey != "license-only" {
		t.Fatalf("Registry default: %#v %v", options, err)
	}
	engine.AI.Gateway = config.AgentGatewayConfig{BaseURL: "https://model.example/v1", Model: "custom", APIKeyEnv: "FUSED_TEST_MODEL_KEY"}
	t.Setenv("FUSED_TEST_MODEL_KEY", "custom-key")
	options, err = fusedAgentOptions(engine, "identity")
	if err != nil || options.GatewayKey != "custom-key" || options.RegistryGateway != "false" {
		t.Fatalf("custom provider inherited license: %v", err)
	} // Custom mode selects exactly its own credential.
}

// TestAgentRegistryGatewayPaths covers production defaults and installations mounted beneath a path prefix.
func TestAgentRegistryGatewayPaths(t *testing.T) {
	for _, test := range []struct{ endpoint, gateway string }{
		{"https://registry.usefused.com/graphql", "https://registry.usefused.com/agent/v1"},
		{"https://registry.example/graphql/", "https://registry.example/agent/v1"},
		{"https://registry.example", "https://registry.example/agent/v1"},
		{"https://registry.example/", "https://registry.example/agent/v1"},
		{"https://registry.example/api/graphql/", "https://registry.example/api/agent/v1"},
		{"https://registry.example/api/", "https://registry.example/api/agent/v1"},
		{"http://127.0.0.1:9000/graphql", "http://127.0.0.1:9000/agent/v1"},
	} {
		// Each endpoint shape must preserve its origin and deployment prefix.
		t.Run(test.endpoint, func(t *testing.T) {
			options, err := fusedAgentOptions(config.EngineConfig{RegistryEndpoint: test.endpoint, LicenseKey: "test-license"}, "identity")
			// Incorrect normalization sends completions to an unregistered Registry route.
			if err != nil || options.GatewayURL != test.gateway {
				t.Fatalf("gateway = %q, want %q; error: %v", options.GatewayURL, test.gateway, err)
			}
		})
	}
}
