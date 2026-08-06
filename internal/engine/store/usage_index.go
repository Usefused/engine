package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/models"
)

// WorkspaceSDKServiceImpacts builds a per-service, per-version index of
// which SDK/MCP config keys reference each (serviceID, serviceVersionID)
// pair, by reading every SDK/MCP AppRuntime's Selections and
// cross-referencing config states (config_key -> latest_resource_id -> app
// runtime). Originally lived in
// internal/engine/api/workspace_config_handlers.go, used only by the
// remove_service/disable_service_version apply-plan-impact check -- moved
// here (a mechanical relocation, not a design change) so
// internal/engine/sandbox's changelog matcher can reuse it too:
// api already imports sandbox, so sandbox importing api back would cycle,
// but both already import store.
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
// enough for changelog-derived version/changed matching, which needs to know
// *which specific endpoints* (SelectAll vs. EndpointIDs/OperationNames) each
// config actually selected, to narrow an endpoint-diff notification down to
// configs that selected the endpoint that changed.
type WorkspaceSDKSelectionMatch struct {
	ConfigKey string
	Selection models.SDKSelection
}

// WorkspaceSDKSelectionsByServiceVersion is WorkspaceSDKServiceImpacts'
// detail-preserving base: same underlying config-state/app-runtime
// fetch, but keeps each config's full SDKSelection instead of collapsing
// straight to a config-key list.
func WorkspaceSDKSelectionsByServiceVersion(ctx context.Context, configStore ConfigRepository, s Store) (map[uuid.UUID]map[uuid.UUID][]WorkspaceSDKSelectionMatch, error) {
	states, err := configStore.ListConfigStates(ctx, ConfigTypeSDK)
	if err != nil {
		return nil, err
	}
	appIDs, configKeys := appRuntimeStateIndex(states)
	scopes, err := s.ListAppRuntimes(ctx, appIDs)
	if err != nil {
		return nil, err
	}
	return workspaceSDKSelectionsFromScopes(scopes, configKeys)
}

func appRuntimeStateIndex(states []ConfigState) ([]uuid.UUID, map[uuid.UUID][]string) {
	configKeys := make(map[uuid.UUID][]string)
	for _, state := range states {
		if state.LatestResourceID == nil {
			continue
		}
		configKeys[*state.LatestResourceID] = append(configKeys[*state.LatestResourceID], state.ConfigKey)
	}
	appIDs := make([]uuid.UUID, 0, len(configKeys))
	for appID := range configKeys {
		appIDs = append(appIDs, appID)
	}
	return appIDs, configKeys
}

func workspaceSDKSelectionsFromScopes(scopes map[uuid.UUID]*AppRuntime, configKeys map[uuid.UUID][]string) (map[uuid.UUID]map[uuid.UUID][]WorkspaceSDKSelectionMatch, error) {
	out := make(map[uuid.UUID]map[uuid.UUID][]WorkspaceSDKSelectionMatch)
	for appID, scope := range scopes {
		if scope == nil {
			continue
		}
		var selections []models.SDKSelection
		if err := json.Unmarshal(scope.Selections, &selections); err != nil {
			return nil, fmt.Errorf("decode sdk scope selections for %s: %w", appID, err)
		}
		for _, selection := range selections {
			if out[selection.ServiceID] == nil {
				out[selection.ServiceID] = make(map[uuid.UUID][]WorkspaceSDKSelectionMatch)
			}
			for _, configKey := range configKeys[appID] {
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
