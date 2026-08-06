package api

import (
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
)

// MountConfigRoutes attaches all the config-as-code endpoints to the main Engine router.
func MountConfigRoutes(r chi.Router, configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, proxy Forwarder, registryClient sandbox.RegistryClient, masterKey []byte, revisionSinks ...authorizationRevisionSink) {
	revisionSink := firstAuthorizationRevisionSink(revisionSinks)
	revisionLoader, _ := s.(accesscontrol.AuthorizationRevisionLoader)
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
	packageClient, _ := registryClient.(SDKPackageClient)
	r.Route("/sdk-config", func(r chi.Router) {
		r.Post("/plan", SDKConfigPlanHandler(configStore, s, registryClient))
		r.Post("/apply", authorizationRevisionSyncHandler(revisionLoader, revisionSink, SDKConfigApplyHandler(configStore, s, proxy, registryClient)))
	})
	r.Get("/sdks/{app_id}/download", SDKPackageDownloadHandler(s, proxy, packageClient))

	// SDK and MCP versions share lifecycle semantics. There is deliberately no
	// activate/reactivate endpoint: deactivation is an irreversible hard delete.
	r.Route("/apps/{app_id}", func(r chi.Router) {
		r.Post("/deprecate", DeprecateAppHandler(s))
		r.Post("/undeprecate", UndeprecateAppHandler(s))
		r.Delete("/", DeactivateAppHandler(s, proxy))
	})

	// MCP uses the same desired-state contract and resolver as SDK configs,
	// but its apply executor creates an Engine runtime instead of source code.
	r.Route("/mcp-config", func(r chi.Router) {
		r.Post("/plan", MCPConfigPlanHandler(configStore, s, registryClient))
		r.Post("/apply", authorizationRevisionSyncHandler(revisionLoader, revisionSink, MCPConfigApplyHandler(configStore, s, registryClient)))
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
