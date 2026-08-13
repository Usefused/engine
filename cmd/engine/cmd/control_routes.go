package cmd

import (
	"github.com/go-chi/chi/v5"

	"github.com/Usefused/engine/internal/engine/api"
)

// registerNativeRESTControlRoutes is shared by production wiring and policy
// coverage tests, so adding a native REST endpoint without a policy fails CI.
func registerNativeRESTControlRoutes(r chi.Router, deps engineRouterDeps) {
	enginePublicGRPCURL := ""
	if deps.cfg != nil {
		enginePublicGRPCURL = deps.cfg.Engine.PublicGRPCURL
	}
	r.Get("/audit/export", api.AuditExportHandler(deps.engineStore))
	r.Mount("/workspace", api.WorkspaceHandler(deps.engineStore, deps.registryClient, deps.masterKey, deps.appTokenRevoker))
	api.MountConfigRoutes(r, deps.configStore, deps.engineStore, deps.registryClient, deps.registryProxy, deps.registryClient, deps.masterKey, enginePublicGRPCURL, deps.controlAuth)
}
