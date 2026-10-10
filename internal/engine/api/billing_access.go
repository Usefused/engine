package api

import (
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"net/http"
)

// BillingAccessHandler projects the authenticated actor's existing grant without adding another permission system.
func BillingAccessHandler(w http.ResponseWriter, r *http.Request) {
	actor, ok := accesscontrol.ActorFromContext(r.Context())
	// Defense in depth preserves authentication when a router mounts this handler incorrectly.
	if !ok {
		accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrAuthenticationRequired)
		return
	}
	requirement := accesscontrol.Requirement{Permission: accesscontrol.PermissionBillingManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}}
	allowed := (accesscontrol.SnapshotAuthorizer{}).CheckAll(r.Context(), actor, requirement) == nil
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]bool{"can_manage": allowed})
}
