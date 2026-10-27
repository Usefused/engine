package bridgehost

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// MountHTTP keeps pairing mutations on the Engine control plane; sessions use separate credentials.
func (s *Server) MountHTTP(router chi.Router, publicURL, grpcURL string) {
	s.grpcURL = grpcURL
	base := strings.TrimRight(publicURL, "/")
	s.discoveryReady = validBridgeEndpoint(base, false) && validBridgeEndpoint(grpcURL, true)
	router.Get("/bridges/discovery", func(w http.ResponseWriter, r *http.Request) {
		// Missing or unsafe endpoint configuration must fail before a device attempts consent.
		if !s.discoveryReady {
			respond(w, 503, map[string]string{"error": "bridge_endpoints_unavailable"})
			return
		}
		// Discovery returns configured origins, never untrusted Host-derived endpoints.
		respond(w, http.StatusOK, map[string]any{"protocol_major": 1, "protocol_minor": 1, "scope": "workspace", "grpc_url": grpcURL, "authorization_endpoint": base + "/oauth/authorize", "token_endpoint": base + "/oauth/token", "registration_endpoint": base + "/oauth/register", "pairing_endpoint": base + "/bridges/pairings", "scopes": []string{"bridge.manage"}})
	})
	router.Post("/bridges/pairings", s.createPairing)
	router.Post("/bridges/pairings/{pairing_id}/revoke", s.selfRevoke)
	router.Get("/bridges", s.listPairings)
	router.Delete("/bridges/{pairing_id}", s.revokePairing)
	router.Put("/bridges/{pairing_id}/profiles", s.updateProfiles)
	router.Get("/bridges/{pairing_id}/manifests", s.listManifests)
}

// controlActor applies defense-in-depth authorization even when handlers are mounted independently.
func controlActor(w http.ResponseWriter, r *http.Request, permission accesscontrol.Permission) (accesscontrol.Actor, bool) {
	actor, ok := accesscontrol.ActorFromContext(r.Context())
	// Installation credentials are deliberately not part of the control authenticator.
	if !ok {
		respond(w, 401, map[string]string{"error": "authentication_required"})
		return actor, false
	}
	err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(r.Context(), actor, accesscontrol.Requirement{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}})
	// No handler may substitute possession of an identifier for workspace authority.
	if err != nil {
		respond(w, 403, map[string]string{"error": "permission_denied"})
		return actor, false
	}
	return actor, true
}

// createPairing exchanges consented OAuth authority into a narrowly bound installation credential.
func (s *Server) createPairing(w http.ResponseWriter, r *http.Request) {
	actor, ok := controlActor(w, r, accesscontrol.PermissionBridgeManage)
	// Stop before decoding or issuing credentials when consent lacks the pairing scope.
	if !ok {
		return
	}
	// Do not issue a credential that can only be sent to an unusable or unsafe advertised endpoint.
	if !s.discoveryReady {
		respond(w, 503, map[string]string{"error": "bridge_endpoints_unavailable"})
		return
	}
	var input struct {
		InstallationID string `json:"installation_id"`
		Name           string `json:"name"`
		Scope          string `json:"scope"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	// Invalid pairing metadata cannot silently expand profile access.
	if decoder.Decode(&input) != nil {
		respond(w, 400, map[string]string{"error": "invalid_pairing"})
		return
	}
	p, secret, err := s.repo.Create(r.Context(), actor, input.InstallationID, input.Name, input.Scope)
	// Database or validation failures never expose generated credentials.
	if err != nil {
		respond(w, 400, map[string]string{"error": "pairing_failed"})
		return
	}
	respond(w, 201, map[string]any{"pairing_id": p.ID, "destination_id": p.DestinationID, "installation_id": p.InstallationID, "credential": secret, "grpc_url": s.grpcURL, "scope": "workspace"})
}

// listPairings projects sanitized workspace identities without exposing credential hashes.
func (s *Server) listPairings(w http.ResponseWriter, r *http.Request) {
	actor, ok := controlActor(w, r, accesscontrol.PermissionBridgeRead)
	// Read authority is checked independently of mutation authority.
	if !ok {
		return
	}
	rows, err := s.repo.Pool.Query(r.Context(), `SELECT id,destination_id,installation_id,name,scope,ARRAY(SELECT profile_id FROM fused_bridge_profiles c WHERE c.pairing_id=p.id AND c.state<>'removed' ORDER BY profile_id),account_id,workspace_id,generation FROM fused_bridge_pairings p WHERE workspace_id=$1 AND revoked_at IS NULL ORDER BY created_at LIMIT 1000`, actor.WorkspaceID)
	// Storage failures must not masquerade as an empty catalogue.
	if err != nil {
		respond(w, 503, map[string]string{"error": "storage_unavailable"})
		return
	}
	defer rows.Close()
	result := []Pairing{}
	for rows.Next() {
		var p Pairing
		// Stop on corrupt rows rather than returning a partial authorization view.
		if rows.Scan(&p.ID, &p.DestinationID, &p.InstallationID, &p.Name, &p.Scope, &p.Profiles, &p.AccountID, &p.WorkspaceID, &p.Generation) != nil {
			respond(w, 503, map[string]string{"error": "storage_unavailable"})
			return
		}
		result = append(result, p)
	}
	// Iteration failures cannot be reported as success.
	if rows.Err() != nil {
		respond(w, 503, map[string]string{"error": "storage_unavailable"})
		return
	}
	respond(w, 200, map[string]any{"bridges": result})
}

// revokePairing fences active sessions and preserves pending delivery evidence.
func (s *Server) revokePairing(w http.ResponseWriter, r *http.Request) {
	actor, ok := controlActor(w, r, accesscontrol.PermissionBridgeManage)
	// Revocation is a workspace-authorized operation, not self-asserted by the device.
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "pairing_id"))
	// Malformed IDs are rejected before SQL casting.
	if err != nil {
		respond(w, 400, map[string]string{"error": "invalid_pairing"})
		return
	}
	result, err := s.repo.Pool.Exec(r.Context(), `UPDATE fused_bridge_pairings SET revoked_at=COALESCE(revoked_at,now()),generation=generation+1,lease_until=NULL WHERE id=$1 AND workspace_id=$2`, id, actor.WorkspaceID)
	// Cross-workspace identifiers cannot disclose whether a pairing exists.
	if err != nil || result.RowsAffected() != 1 {
		respond(w, 404, map[string]string{"error": "pairing_not_found"})
		return
	}
	respond(w, 200, map[string]bool{"revoked": true})
}

// updateProfiles rejects obsolete selection updates instead of pretending they constrain workspace consent.
func (s *Server) updateProfiles(w http.ResponseWriter, r *http.Request) {
	_, ok := controlActor(w, r, accesscontrol.PermissionBridgeManage)
	// Even obsolete control actions require ordinary workspace authority.
	if !ok {
		return
	}
	respond(w, http.StatusConflict, map[string]string{"error": "workspace_scope_required", "message": "Profiles are discovered automatically; reconnect legacy pairings with workspace consent."})
}

// listManifests exposes accepted current contracts for selection without provider credentials.
func (s *Server) listManifests(w http.ResponseWriter, r *http.Request) {
	actor, ok := controlActor(w, r, accesscontrol.PermissionBridgeRead)
	// Catalogue visibility remains workspace scoped.
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "pairing_id"))
	if err != nil {
		respond(w, 400, map[string]string{"error": "invalid_pairing"})
		return
	}
	rows, err := s.repo.Pool.Query(r.Context(), `SELECT c.profile_id,COALESCE(m.revision,''),COALESCE(m.hash,''),COALESCE(m.document,convert_to('{"operations":[]}','UTF8')),CASE WHEN c.state='online' AND (p.lease_until IS NULL OR p.lease_until<=now() OR p.inventory_generation<>p.generation) THEN 'offline' ELSE c.state END FROM fused_bridge_profiles c LEFT JOIN fused_bridge_manifests m ON m.pairing_id=c.pairing_id AND m.profile_id=c.profile_id AND m.hash=c.manifest_hash JOIN fused_bridge_pairings p ON p.id=c.pairing_id WHERE p.id=$1 AND p.workspace_id=$2 AND p.revoked_at IS NULL AND p.scope='workspace' AND c.state<>'removed' ORDER BY c.profile_id`, id, actor.WorkspaceID)
	// No partial catalogue is returned during a storage outage.
	if err != nil {
		respond(w, 503, map[string]string{"error": "storage_unavailable"})
		return
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var raw []byte
		var profile, revision, hash, state string
		// Invalid stored contracts fail the complete catalogue response.
		if rows.Scan(&profile, &revision, &hash, &raw, &state) != nil {
			respond(w, 503, map[string]string{"error": "storage_unavailable"})
			return
		}
		result = append(result, map[string]any{"profile_id": profile, "revision": revision, "manifest_hash": hash, "document": json.RawMessage(raw), "state": state})
	}
	if rows.Err() != nil {
		respond(w, 503, map[string]string{"error": "storage_unavailable"})
		return
	}
	respond(w, 200, map[string]any{"manifests": result})
}

// respond prevents credentials and catalogue responses from entering shared HTTP caches.
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// selfRevoke allows a device to destroy only its own installation grant.
func (s *Server) selfRevoke(w http.ResponseWriter, r *http.Request) {
	raw := r.Header.Get("Authorization")
	// No general control credential or anonymous identifier can impersonate a device.
	if !strings.HasPrefix(raw, "Bearer ") {
		respond(w, 401, map[string]string{"error": "authentication_required"})
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "pairing_id"))
	// A device may retry revocation after its own grant or owner permissions have already disappeared.
	if err != nil {
		respond(w, 401, map[string]string{"error": "authentication_required"})
		return
	}
	secret := strings.TrimPrefix(raw, "Bearer ")
	// The credential hash check is deliberately local to this authority-reducing operation.
	result, err := s.repo.Pool.Exec(r.Context(), `UPDATE fused_bridge_pairings SET revoked_at=COALESCE(revoked_at,now()),generation=generation+1,lease_until=NULL WHERE id=$1 AND credential_hash=$2`, id, Digest([]byte(secret)))
	// Unknown credentials never disclose whether the pairing was already revoked.
	if err != nil {
		respond(w, 503, map[string]string{"error": "storage_unavailable"})
		return
	}
	if result.RowsAffected() != 1 {
		respond(w, 401, map[string]string{"error": "authentication_required"})
		return
	}
	respond(w, 200, map[string]bool{"revoked": true})
}
