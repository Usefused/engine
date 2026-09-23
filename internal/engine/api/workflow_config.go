package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type workflowSource struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Hash    string `json:"hash"`
}

var workflowHashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// validateWorkflowSources bounds provenance without mistaking it for trusted execution authority.
func validateWorkflowSources(sources []workflowSource) error {
	// The library selector's bound applies equally to hand-authored app configs.
	if len(sources) > 32 {
		return fmt.Errorf("at most 32 workflow sources are supported")
	}
	seen := map[string]bool{}
	for _, source := range sources {
		id, err := uuid.Parse(source.ID)
		// One source must identify one immutable release with a full digest.
		if err != nil || id == uuid.Nil || seen[source.ID] || source.Version == "" || len(source.Version) > 128 || !workflowHashPattern.MatchString(source.Hash) {
			return fmt.Errorf("invalid or duplicate workflow source")
		}
		seen[source.ID] = true
	}
	return nil
}

// AppConfigSourceHandler returns Engine-owned source to authorized editors without contacting Registry.
func AppConfigSourceHandler(s store.Store, configs store.ConfigRepository) http.HandlerFunc {
	// Authentication and edit authority are checked before private authoring is loaded.
	return func(w http.ResponseWriter, r *http.Request) {
		actor, app, err := lifecycleActorAndApp(r.Context(), s, r)
		// A failed identity lookup must not reach private source storage.
		if err != nil {
			writeSDKConfigError(w, err)
			return
		}
		// Public app metadata visibility does not grant access to executable mappings.
		if err := authorizeWorkflowSource(r.Context(), s, actor, app); err != nil {
			writeSDKConfigError(w, err)
			return
		}
		state, err := loadExactWorkflowSource(r.Context(), configs, app)
		// Missing exact source cannot fall back to a descriptor reconstruction.
		if err != nil {
			writeSDKConfigError(w, err)
			return
		}
		ownerTeam, err := workflowSourceOwnerTeam(r.Context(), s, state)
		// A successor must preserve authoritative ownership rather than guessing a team.
		if err != nil {
			writeSDKConfigError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"app_id": app.AppID, "config": state.DesiredState, "owner_team": ownerTeam})
	}
}

// authorizeWorkflowSource derives type-specific edit authority from the trusted immutable family.
func authorizeWorkflowSource(ctx context.Context, s store.Store, actor accesscontrol.Actor, app *store.App) error {
	family, err := s.GetAppFamily(ctx, app.AppFamilyID)
	// A version cannot nominate a different account or permission namespace.
	if err != nil || family == nil || family.AccountID != actor.AccountID || family.AppFamilyID != app.AppFamilyID {
		return workspaceConfigHTTPError{status: http.StatusNotFound, message: "app family unavailable"}
	}
	requirement := accesscontrol.Requirement{Permission: accesscontrol.AppPermission(store.AppPermissionType(family), accesscontrol.PermissionAppManage), Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: app.AppFamilyID}}
	return (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, requirement)
}

// loadExactWorkflowSource rejects missing, stale, or oversized source instead of reconstructing private config from runtime descriptors.
func loadExactWorkflowSource(ctx context.Context, configs store.ConfigRepository, app *store.App) (*store.ConfigState, error) {
	state, err := configs.GetConfigState(ctx, app.ConfigKey)
	// Exact resource identity prevents returning a sibling or a stale desired-state row.
	if err != nil || state == nil || state.LatestResourceID == nil || *state.LatestResourceID != app.AppID {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, message: "exact app configuration is unavailable"}
	}
	// Bound private source exports independently of the caller transport.
	if len(state.DesiredState) > 1<<20 {
		return nil, workspaceConfigHTTPError{status: http.StatusRequestEntityTooLarge, message: "app configuration exceeds export limit"}
	}
	return state, nil
}

// workflowSourceOwnerTeam retains the immutable owner when an administrator prepares a successor version.
func workflowSourceOwnerTeam(ctx context.Context, s store.Store, state *store.ConfigState) (string, error) {
	// Subject-owned apps have no team override in their next plan request.
	if state.OwnerTeamID == nil {
		return "", nil
	}
	repo, ok := s.(store.TeamRepository)
	// Missing team lookup must not silently change ownership to the current subject.
	if !ok {
		return "", workspaceConfigHTTPError{status: http.StatusServiceUnavailable, message: "team lookup unavailable"}
	}
	team, err := repo.GetTeam(ctx, *state.OwnerTeamID)
	// A failed owner lookup prevents exporting an incomplete successor configuration.
	if err != nil {
		return "", err
	}
	return team.Slug, nil
}
