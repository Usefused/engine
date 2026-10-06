package cmd

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/agentbundle"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/go-chi/chi/v5"
)

var agentRoutes = []struct{ method, path string }{
	{"GET", "/agent/status"}, {"GET", "/agent/sessions"}, {"POST", "/agent/sessions"},
	{"GET", "/agent/sessions/{id}"}, {"DELETE", "/agent/sessions/{id}"},
	{"GET", "/agent/sessions/{id}/messages"}, {"POST", "/agent/responses"}, {"POST", "/agent/client-tools/{id}"},
	// The runtime binds each decision to its authenticated actor and exact suspended call.
	{"POST", "/agent/approvals/{id}"},
}

// mountFusedAgent keeps conversation transport authenticated while draft tools use the browser's existing API permissions.
func mountFusedAgent(r chi.Router, deps engineRouterDeps) {
	cfg := deps.cfg.Engine.AI
	identityBytes := make([]byte, 32)
	_, err := rand.Read(identityBytes)
	// A missing signing key must disable the agent rather than create anonymous sessions.
	if err != nil {
		panic("unable to initialize agent identity")
	}
	identityKey := hex.EncodeToString(identityBytes)
	var manager *agentbundle.Manager
	setupError := ""
	// The optional bundled runtime starts independently from Engine readiness.
	if cfg.Enabled && deps.ctx != nil {
		options, err := fusedAgentOptions(deps.cfg.Engine, identityKey)
		// Incomplete configuration or runtime state must not grant an unverified agent connection.
		if err != nil {
			setupError = err.Error()
		} else {
			manager, err = agentbundle.New(options)
			// Incomplete configuration or runtime state must not grant an unverified agent connection.
			if err != nil {
				setupError = err.Error()
			} else {
				manager.Start(deps.ctx)
				go func() { <-deps.ctx.Done(); _ = manager.Close() }()
			}
		}
	}
	// Every mounted route is exact and included in the control authorization manifest.
	for _, route := range agentRoutes {
		r.Method(route.method, route.path, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			endpoint := ""
			var transport http.RoundTripper
			state := "disabled"
			// Incomplete configuration or runtime state must not grant an unverified agent connection.
			if cfg.Enabled {
				state = "unavailable"
				// Incomplete configuration or runtime state must not grant an unverified agent connection.
				if manager != nil {
					state = manager.Status().State
					endpoint, transport = manager.Endpoint()
				}
			}
			// Incomplete configuration or runtime state must not grant an unverified agent connection.
			if req.URL.Path == "/agent/status" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"enabled": cfg.Enabled, "status": state, "message": setupError})
				return
			}
			// Incomplete configuration or runtime state must not grant an unverified agent connection.
			if !cfg.Enabled || endpoint == "" {
				http.Error(w, "Fused agent is unavailable. Check engine.ai configuration and runtime status.", http.StatusServiceUnavailable)
				return
			}
			serveFusedAgent(w, req, endpoint, transport, identityKey)
		}))
	}
}

// fusedAgentOptions selects one provider without forwarding Engine database or encryption credentials.
func fusedAgentOptions(engine config.EngineConfig, identity string) (agentbundle.Options, error) {
	ai := engine.AI
	options := agentbundle.Options{EngineURL: "http://127.0.0.1", Version: Version, CacheDir: ai.CacheDir, ArchivePath: ai.RuntimeArchive, IdentityKey: identity, Model: "fused-agent", RegistryGateway: "true", GatewayURL: strings.TrimRight(engine.RegistryEndpoint, "/") + "/agent/v1", GatewayKey: engine.LicenseKey}
	// A custom gateway never receives the Engine license, even when its credential setting is incomplete.
	if ai.Gateway.BaseURL != "" {
		options.GatewayURL = ai.Gateway.BaseURL
		options.Model = ai.Gateway.Model
		options.RegistryGateway = "false"
		options.GatewayKey = os.Getenv(ai.Gateway.APIKeyEnv)
		// Incomplete configuration or runtime state must not grant an unverified agent connection.
		if options.Model == "" || options.GatewayKey == "" {
			return options, fmt.Errorf("custom agent gateway requires a model and a populated api_key_env")
		}
	}
	// Incomplete configuration or runtime state must not grant an unverified agent connection.
	if err := config.ValidateAgentURL(options.GatewayURL); err != nil {
		return options, err
	}
	return options, nil
}

// serveFusedAgent replaces browser credentials with a short-lived identity usable only by this agent runtime.
func serveFusedAgent(w http.ResponseWriter, r *http.Request, endpoint string, transport http.RoundTripper, key string) {
	actor, ok := accesscontrol.ActorFromContext(r.Context())
	// Defence in depth prevents a future route mount from bypassing the normal control-plane actor middleware.
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	target, err := url.Parse(endpoint)
	// Incomplete configuration or runtime state must not grant an unverified agent connection.
	if err != nil {
		http.Error(w, "invalid agent endpoint", http.StatusServiceUnavailable)
		return
	}
	proxy := httputil.ReverseProxy{Transport: transport, FlushInterval: -1, Rewrite: func(p *httputil.ProxyRequest) {
		p.SetURL(target)
		p.Out.URL.Path = strings.TrimPrefix(p.In.URL.Path, "/agent")
		p.Out.URL.RawPath = ""
		p.Out.Header = make(http.Header)
		p.Out.Header.Set("Content-Type", p.In.Header.Get("Content-Type"))
		p.Out.Header.Set("Accept", p.In.Header.Get("Accept"))
		p.Out.Header.Set("Authorization", "Bearer "+fusedAgentIdentity(actor, key, time.Now()))
	}, ModifyResponse: func(response *http.Response) error {
		// Redirects must never send actor identity to a different host.
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			return fmt.Errorf("agent redirects are unsupported")
		}
		response.Header.Del("Set-Cookie")
		response.Header.Set("Cache-Control", "no-store")
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "Fused agent connection failed", http.StatusBadGateway)
	}}
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	proxy.ServeHTTP(w, r)
}

// fusedAgentIdentity binds each conversation to the verified workspace actor, not a browser-supplied session owner.
func fusedAgentIdentity(actor accesscontrol.Actor, key string, now time.Time) string {
	subject := actor.AccountID.String() + ":" + actor.WorkspaceID.String() + ":" + string(actor.Kind) + ":" + actor.SubjectID.String()
	claims, _ := json.Marshal(map[string]any{"sub": subject, "workspace_id": actor.WorkspaceID.String(), "iss": "fused-engine", "aud": "fused-agent", "iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix()})
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
