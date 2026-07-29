package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/models"
)

// artifactScopeBatchReader is the optional targeted-batch capability
// WorkspaceSDKServiceImpacts prefers when the concrete Store supports it
// (postgresStore.ListArtifactScopes), falling back to one GetArtifactScope
// call per artifact ID otherwise -- the same optional-capability pattern
// used throughout this package (e.g. WorkspaceProfileStore).
type artifactScopeBatchReader interface {
	ListArtifactScopes(ctx context.Context, artifactIDs []uuid.UUID) (map[uuid.UUID]*ArtifactScope, error)
}

// WorkspaceSDKServiceImpacts builds a per-service, per-version index of
// which SDK/MCP config keys reference each (serviceID, serviceVersionID)
// pair, by reading every SDK/MCP ArtifactScope's Selections and
// cross-referencing config states (config_key -> latest_resource_id ->
// artifact scope). Originally lived in
// internal/engine/api/workspace_config_handlers.go, used only by the
// remove_service/disable_service_version apply-plan-impact check -- moved
// here (a mechanical relocation, not a design change) so
// internal/engine/sandbox's Phase 3 changelog matcher can reuse it too:
// api already imports sandbox, so sandbox importing api back would cycle,
// but both already import store (see plans/plan-service-changelog.md's
// "## Phase 3", "Where the usage index lives").
//
// This is a thin projection of WorkspaceSDKSelectionsByServiceVersion down
// to just config keys -- kept as its own function (rather than inlining a
// collapse at every call site) because it's the shape the existing
// apply-plan-impact caller already depends on and is tested against.
func WorkspaceSDKServiceImpacts(ctx context.Context, configStore ConfigRepository, s Store) (map[uuid.UUID]map[uuid.UUID][]string, error) {
	detailed, err := WorkspaceSDKSelectionsByServiceVersion(ctx, configStore, s)
	if err != nil {
		return nil, err
	}
	return collapseToConfigKeys(detailed), nil
}

// WorkspaceSDKSelectionMatch pairs one SDK/MCP config's key with the raw
// SDKSelection it made for one service+version. WorkspaceSDKServiceImpacts
// alone only keeps the coarse config-key list, which is enough to know
// "this config touches this service+version at all" but not enough for
// Phase 3's version/changed matching, which needs to know *which specific
// endpoints* (SelectAll vs. EndpointIDs/OperationNames) each config
// actually selected, to narrow an endpoint-diff notification down to
// configs that selected the endpoint that changed.
type WorkspaceSDKSelectionMatch struct {
	ConfigKey string
	Selection models.SDKSelection
}

// WorkspaceSDKSelectionsByServiceVersion is WorkspaceSDKServiceImpacts'
// detail-preserving base: same underlying config-state/artifact-scope
// fetch, but keeps each config's full SDKSelection instead of collapsing
// straight to a config-key list.
func WorkspaceSDKSelectionsByServiceVersion(ctx context.Context, configStore ConfigRepository, s Store) (map[uuid.UUID]map[uuid.UUID][]WorkspaceSDKSelectionMatch, error) {
	states, err := configStore.ListConfigStates(ctx, ConfigTypeSDK)
	if err != nil {
		return nil, err
	}
	artifactIDs, configKeys := artifactScopeStateIndex(states)
	scopes, err := workspaceArtifactScopes(ctx, s, artifactIDs)
	if err != nil {
		return nil, err
	}
	return workspaceSDKSelectionsFromScopes(scopes, configKeys)
}

func artifactScopeStateIndex(states []ConfigState) ([]uuid.UUID, map[uuid.UUID][]string) {
	configKeys := make(map[uuid.UUID][]string)
	for _, state := range states {
		if state.LatestResourceID == nil {
			continue
		}
		configKeys[*state.LatestResourceID] = append(configKeys[*state.LatestResourceID], state.ConfigKey)
	}
	artifactIDs := make([]uuid.UUID, 0, len(configKeys))
	for artifactID := range configKeys {
		artifactIDs = append(artifactIDs, artifactID)
	}
	return artifactIDs, configKeys
}

func workspaceArtifactScopes(ctx context.Context, s Store, artifactIDs []uuid.UUID) (map[uuid.UUID]*ArtifactScope, error) {
	if batchStore, ok := s.(artifactScopeBatchReader); ok {
		return batchStore.ListArtifactScopes(ctx, artifactIDs)
	}
	return workspaceArtifactScopesFallback(ctx, s, artifactIDs)
}

func workspaceArtifactScopesFallback(ctx context.Context, s Store, artifactIDs []uuid.UUID) (map[uuid.UUID]*ArtifactScope, error) {
	out := make(map[uuid.UUID]*ArtifactScope)
	for _, artifactID := range artifactIDs {
		scope, err := s.GetArtifactScope(ctx, artifactID)
		if errors.Is(err, ErrArtifactScopeNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[artifactID] = scope
	}
	return out, nil
}

func workspaceSDKSelectionsFromScopes(scopes map[uuid.UUID]*ArtifactScope, configKeys map[uuid.UUID][]string) (map[uuid.UUID]map[uuid.UUID][]WorkspaceSDKSelectionMatch, error) {
	out := make(map[uuid.UUID]map[uuid.UUID][]WorkspaceSDKSelectionMatch)
	for artifactID, scope := range scopes {
		if scope == nil {
			continue
		}
		var selections []models.SDKSelection
		if err := json.Unmarshal(scope.Selections, &selections); err != nil {
			return nil, fmt.Errorf("decode sdk scope selections for %s: %w", artifactID, err)
		}
		for _, selection := range selections {
			if out[selection.ServiceID] == nil {
				out[selection.ServiceID] = make(map[uuid.UUID][]WorkspaceSDKSelectionMatch)
			}
			for _, configKey := range configKeys[artifactID] {
				out[selection.ServiceID][selection.ServiceVersionID] = append(
					out[selection.ServiceID][selection.ServiceVersionID],
					WorkspaceSDKSelectionMatch{ConfigKey: configKey, Selection: selection},
				)
			}
		}
	}
	return out, nil
}

// collapseToConfigKeys projects the detailed selection index down to
// WorkspaceSDKServiceImpacts' sorted config-key-only shape, preserving that
// function's existing, already-tested output exactly.
func collapseToConfigKeys(detailed map[uuid.UUID]map[uuid.UUID][]WorkspaceSDKSelectionMatch) map[uuid.UUID]map[uuid.UUID][]string {
	out := make(map[uuid.UUID]map[uuid.UUID][]string, len(detailed))
	for serviceID, byVersion := range detailed {
		out[serviceID] = make(map[uuid.UUID][]string, len(byVersion))
		for versionID, matches := range byVersion {
			keys := make([]string, len(matches))
			for i, match := range matches {
				keys[i] = match.ConfigKey
			}
			sort.Strings(keys)
			out[serviceID][versionID] = keys
		}
	}
	return out
}
