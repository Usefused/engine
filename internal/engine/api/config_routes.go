package api

import (
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
)

// MountConfigRoutes attaches all the config-as-code endpoints to the main Engine router.
func MountConfigRoutes(r chi.Router, configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, proxy Forwarder, registryClient sandbox.RegistryClient, masterKey []byte) {
	// Workspace config routes (mounted alongside the existing /workspace routes)
	r.Route("/workspace/config", func(r chi.Router) {
		r.Post("/plan", WorkspaceConfigPlanHandler(configStore, s, verifier))
		r.Post("/apply", WorkspaceConfigApplyHandler(configStore, s, verifier, masterKey))
	})

	// Generic plan action routes
	r.Route("/config/plans", func(r chi.Router) {
		r.Patch("/{planId}/actions", ConfigPlanActionsHandler(configStore, s))
	})

	// SDK config routes
	r.Route("/sdk-config", func(r chi.Router) {
		r.Post("/plan", SDKConfigPlanHandler(configStore, s, registryClient))
		r.Post("/apply", SDKConfigApplyHandler(configStore, s, proxy, registryClient))
		r.Get("/{name}/download", SDKConfigDownloadHandler(configStore, s, proxy))

		// SDK/MCP lifecycle routes. Deliberately under /sdk-config, not
		// /sdks/* -- /sdks/* is proxied straight through to the Registry
		// (see RESTProxyMountPaths), so an Engine-native route registered
		// there would either be shadowed by the proxy or collide with it.
		r.Post("/{id}/activate", ActivateSDKHandler(s))
		r.Post("/{id}/deactivate", DeactivateSDKHandler(s))
		r.Delete("/{id}", DeleteSDKHandler(s))
	})

	// MCP uses the same desired-state contract and resolver as SDK configs,
	// but its apply executor creates an Engine runtime instead of source code.
	r.Route("/mcp-config", func(r chi.Router) {
		r.Post("/plan", MCPConfigPlanHandler(configStore, s, registryClient))
		r.Post("/apply", MCPConfigApplyHandler(configStore, s, registryClient))
	})

	// kind: webhook (plans/plan-webhook-kind.md) -- completely Engine-owned,
	// unlike sdk-config/mcp-config: no registryClient, no proxy, no
	// generation. verifier is the same ServiceVerifier the workspace routes
	// already use, reused here only to fetch each referenced service's
	// webhook auth shape (resolveWebhookAuthShape).
	r.Route("/webhook-config", func(r chi.Router) {
		r.Post("/plan", WebhookConfigPlanHandler(configStore, s, verifier, registryClient))
		r.Post("/apply", WebhookConfigApplyHandler(configStore, s, verifier, registryClient))
	})
}
