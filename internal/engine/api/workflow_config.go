package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/Usefused/engine/internal/shared/models"
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
		pins, err := workflowSourcePins(r.Context(), s, app, state.DesiredState)
		// Export alias identity only when it still matches the immutable app scope.
		if err != nil {
			writeSDKConfigError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"app_id": app.AppID, "config": state.DesiredState, "owner_team": ownerTeam, "service_pins": pins})
	}
}

type workflowServicePin struct {
	Key              string    `json:"key"`
	ServiceID        uuid.UUID `json:"service_id"`
	ServiceVersionID uuid.UUID `json:"service_version_id"`
}

// workflowSourcePins associates saved display-name keys and authored graph aliases with exact app selections, without rebuilding private config.
func workflowSourcePins(ctx context.Context, s store.Store, app *store.App, source json.RawMessage) ([]workflowServicePin, error) {
	var doc sdkConfigDocument
	// Corrupt private source must not become a partial editable configuration.
	if err := json.Unmarshal(source, &doc); err != nil {
		return nil, workflowSourceIdentityError()
	}
	var selections []models.SDKSelection
	// Version identity comes from this immutable app, never the workspace's latest pin.
	if err := json.Unmarshal(app.Selections, &selections); err != nil {
		return nil, workflowSourceIdentityError()
	}
	keys := workflowSourceKeys(doc)
	// Empty source fixtures and operation-free configs need no identity query.
	if len(keys) == 0 {
		return []workflowServicePin{}, nil
	}
	resolved, err := s.ResolveWorkspaceServiceIDsByKeys(ctx, keys)
	// One bounded local lookup rejects missing or ambiguous aliases without Registry fallback.
	if err != nil {
		return nil, err
	}
	pins, err := matchWorkflowSourcePins(keys, resolved, selections)
	// Exact membership must be established before comparing private graph identities.
	if err != nil {
		return nil, err
	}
	return pins, validateWorkflowSourceGraphPins(app, doc, pins)
}

// validateWorkflowSourceGraphPins catches alias swaps even when both providers already belong to the app's immutable scope.
func validateWorkflowSourceGraphPins(app *store.App, doc sdkConfigDocument, pins []workflowServicePin) error {
	// Physical-only apps have no private graph selectors to preserve.
	if len(doc.UnifiedOperations) == 0 {
		return nil
	}
	definitions, err := unified.DecodeDefinitions(app.UnifiedDefinitions, unified.DefaultLimits())
	// Missing executable evidence must not be replaced by public descriptors or live catalogue data.
	if err != nil || len(definitions) != len(doc.UnifiedOperations) {
		return workflowSourceIdentityError()
	}
	byKey := make(map[string]workflowServicePin, len(pins))
	for _, pin := range pins {
		byKey[pin.Key] = pin
	}
	for _, definition := range definitions {
		for _, binding := range definition.Bindings {
			pin := byKey[binding.ServiceTarget]
			// An existing binding must retain both the provider and immutable contract version.
			if pin.ServiceID != binding.ServiceID || pin.ServiceVersionID != binding.ServiceVersionID {
				return workflowSourceIdentityError()
			}
		}
	}
	return nil
}

// workflowSourceKeys includes graph aliases because older desired state saved display names while leaving authored bindings intact.
func workflowSourceKeys(doc sdkConfigDocument) []string {
	keys := make(map[string]bool, len(doc.Services))
	for key := range doc.Services {
		keys[key] = true
	}
	for _, operation := range doc.UnifiedOperations {
		for target, binding := range operation.Bindings {
			keys[unifiedBindingServiceTarget(target, binding.Service)] = true
		}
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

// matchWorkflowSourcePins prevents a renamed or reused local alias from retargeting an existing app during composition.
func matchWorkflowSourcePins(keys []string, resolved map[string]uuid.UUID, selections []models.SDKSelection) ([]workflowServicePin, error) {
	versions := make(map[uuid.UUID]uuid.UUID, len(selections))
	for _, selection := range selections {
		// Multiple versions under one alias cannot be merged without explicit authoring.
		if previous, exists := versions[selection.ServiceID]; exists && previous != selection.ServiceVersionID {
			return nil, workflowSourceIdentityError()
		}
		versions[selection.ServiceID] = selection.ServiceVersionID
	}
	pins := make([]workflowServicePin, 0, len(keys))
	for _, key := range keys {
		id := resolved[key]
		version := versions[id]
		// Unresolved keys and identities outside the immutable scope must fail closed.
		if id == uuid.Nil || version == uuid.Nil {
			return nil, workflowSourceIdentityError()
		}
		pins = append(pins, workflowServicePin{Key: key, ServiceID: id, ServiceVersionID: version})
	}
	return pins, nil
}

// workflowSourceIdentityError gives editors a concrete conflict instead of allowing an alias to silently change provider scope.
func workflowSourceIdentityError() error {
	return workspaceConfigHTTPError{status: http.StatusConflict, message: "exact app service identities are unavailable; refresh the original services before adding workflows"}
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
