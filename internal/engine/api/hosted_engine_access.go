package api

import (
	"encoding/json"
	"net/http"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

// HostedEngineAccessHandler exposes the serving binary version and the existing account-management grant.
func HostedEngineAccessHandler(version string) http.HandlerFunc {
	// The handler reports local authorization without forwarding personal credentials to Registry.
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := accesscontrol.ActorFromContext(r.Context())
		// Missing authentication must not expose a mutation capability.
		if !ok {
			accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrAuthenticationRequired)
			return
		}
		requirement := accesscontrol.Requirement{Permission: accesscontrol.PermissionAccountManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}}
		allowed := (accesscontrol.SnapshotAuthorizer{}).CheckAll(r.Context(), actor, requirement) == nil
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"can_manage": allowed, "running_version": version})
	}
}
