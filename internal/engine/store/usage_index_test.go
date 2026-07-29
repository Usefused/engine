package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/models"
)

// usageIndexMockConfigStore/usageIndexMockStore are narrow test doubles for
// WorkspaceSDKServiceImpacts/WorkspaceSDKSelectionsByServiceVersion --
// intentionally separate from config_repository_test.go's fixtures since
// this file is a regression pin for the api->store extraction (task #38),
// not a general ConfigRepository/Store test suite.
type usageIndexMockConfigStore struct {
	ConfigRepository
	states  []ConfigState
	listErr error
}

func (m *usageIndexMockConfigStore) ListConfigStates(ctx context.Context, configType ConfigType) ([]ConfigState, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.states, nil
}

type usageIndexMockBatchStore struct {
	Store
	scopes    map[uuid.UUID]*ArtifactScope
	batchErr  error
	batchedAt [][]uuid.UUID
}

func (m *usageIndexMockBatchStore) ListArtifactScopes(ctx context.Context, artifactIDs []uuid.UUID) (map[uuid.UUID]*ArtifactScope, error) {
	m.batchedAt = append(m.batchedAt, artifactIDs)
	if m.batchErr != nil {
		return nil, m.batchErr
	}
	out := make(map[uuid.UUID]*ArtifactScope)
	for _, id := range artifactIDs {
		if scope, ok := m.scopes[id]; ok {
			out[id] = scope
		}
	}
	return out, nil
}

// usageIndexMockFallbackStore deliberately does NOT implement
// artifactScopeBatchReader, exercising workspaceArtifactScopesFallback's
// one-GetArtifactScope-per-artifact path.
type usageIndexMockFallbackStore struct {
	Store
	scopes  map[uuid.UUID]*ArtifactScope
	getErrs map[uuid.UUID]error
	gets    []uuid.UUID
}

func (m *usageIndexMockFallbackStore) GetArtifactScope(ctx context.Context, artifactID uuid.UUID) (*ArtifactScope, error) {
	m.gets = append(m.gets, artifactID)
	if err, ok := m.getErrs[artifactID]; ok {
		return nil, err
	}
	scope, ok := m.scopes[artifactID]
	if !ok {
		return nil, ErrArtifactScopeNotFound
	}
	return scope, nil
}

func selectionsJSON(t *testing.T, selections []models.SDKSelection) []byte {
	t.Helper()
	data, err := json.Marshal(selections)
	if err != nil {
		t.Fatalf("marshal selections: %v", err)
	}
	return data
}

// TestWorkspaceSDKServiceImpacts_BatchPath_SortedDedupedConfigKeys is a
// regression pin for the pre-extraction behavior (originally tested only
// indirectly through internal/engine/api's plan-impact handler tests):
// WorkspaceSDKServiceImpacts must still return one sorted config-key list per
// (service, version), using the batched ListArtifactScopes path when the
// concrete Store supports it.
func TestWorkspaceSDKServiceImpacts_BatchPath_SortedDedupedConfigKeys(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	artifactA, artifactB := uuid.New(), uuid.New()

	batchStore := &usageIndexMockBatchStore{scopes: map[uuid.UUID]*ArtifactScope{
		artifactA: {ArtifactID: artifactA, Selections: selectionsJSON(t, []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: versionID}})},
		artifactB: {ArtifactID: artifactB, Selections: selectionsJSON(t, []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: versionID}})},
	}}
	configStore := &usageIndexMockConfigStore{states: []ConfigState{
		{ConfigKey: "sdk:zebra", ConfigType: ConfigTypeSDK, LatestResourceID: &artifactA},
		{ConfigKey: "sdk:apple", ConfigType: ConfigTypeSDK, LatestResourceID: &artifactB},
	}}

	impacts, err := WorkspaceSDKServiceImpacts(context.Background(), configStore, batchStore)
	if err != nil {
		t.Fatalf("WorkspaceSDKServiceImpacts: %v", err)
	}
	keys := impacts[serviceID][versionID]
	if len(keys) != 2 || keys[0] != "sdk:apple" || keys[1] != "sdk:zebra" {
		t.Fatalf("expected sorted [sdk:apple sdk:zebra], got %#v", keys)
	}
	if len(batchStore.batchedAt) != 1 || len(batchStore.batchedAt[0]) != 2 {
		t.Fatalf("expected exactly one batched lookup covering both artifacts, got %#v", batchStore.batchedAt)
	}
}

// TestWorkspaceSDKServiceImpacts_FallsBackToPerArtifactReads proves the
// non-batch Store path (workspaceArtifactScopesFallback) produces the exact
// same shape as the batch path, and that a not-found artifact is skipped
// rather than failing the whole call.
func TestWorkspaceSDKServiceImpacts_FallsBackToPerArtifactReads(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	present, missing := uuid.New(), uuid.New()

	fallbackStore := &usageIndexMockFallbackStore{scopes: map[uuid.UUID]*ArtifactScope{
		present: {ArtifactID: present, Selections: selectionsJSON(t, []models.SDKSelection{{ServiceID: serviceID, ServiceVersionID: versionID}})},
	}}
	configStore := &usageIndexMockConfigStore{states: []ConfigState{
		{ConfigKey: "sdk:present", ConfigType: ConfigTypeSDK, LatestResourceID: &present},
		{ConfigKey: "sdk:missing", ConfigType: ConfigTypeSDK, LatestResourceID: &missing},
	}}

	impacts, err := WorkspaceSDKServiceImpacts(context.Background(), configStore, fallbackStore)
	if err != nil {
		t.Fatalf("WorkspaceSDKServiceImpacts: %v", err)
	}
	keys := impacts[serviceID][versionID]
	if len(keys) != 1 || keys[0] != "sdk:present" {
		t.Fatalf("expected only sdk:present (sdk:missing's artifact is gone), got %#v", keys)
	}
	if len(fallbackStore.gets) != 2 {
		t.Fatalf("expected one GetArtifactScope call per artifact, got %d", len(fallbackStore.gets))
	}
}

// TestWorkspaceSDKSelectionsByServiceVersion_PreservesEndpointDetail is the
// detail Phase 3's endpoint-narrowing depends on: unlike
// WorkspaceSDKServiceImpacts, the detailed base must keep each config's full
// SDKSelection (SelectAll / EndpointIDs), not just its key.
func TestWorkspaceSDKSelectionsByServiceVersion_PreservesEndpointDetail(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	artifactID := uuid.New()
	endpointID := uuid.New()

	batchStore := &usageIndexMockBatchStore{scopes: map[uuid.UUID]*ArtifactScope{
		artifactID: {ArtifactID: artifactID, Selections: selectionsJSON(t, []models.SDKSelection{
			{ServiceID: serviceID, ServiceVersionID: versionID, EndpointIDs: []uuid.UUID{endpointID}},
		})},
	}}
	configStore := &usageIndexMockConfigStore{states: []ConfigState{
		{ConfigKey: "sdk:restricted", ConfigType: ConfigTypeSDK, LatestResourceID: &artifactID},
	}}

	detailed, err := WorkspaceSDKSelectionsByServiceVersion(context.Background(), configStore, batchStore)
	if err != nil {
		t.Fatalf("WorkspaceSDKSelectionsByServiceVersion: %v", err)
	}
	matches := detailed[serviceID][versionID]
	if len(matches) != 1 || matches[0].ConfigKey != "sdk:restricted" {
		t.Fatalf("expected one match for sdk:restricted, got %#v", matches)
	}
	if len(matches[0].Selection.EndpointIDs) != 1 || matches[0].Selection.EndpointIDs[0] != endpointID {
		t.Fatalf("expected the raw EndpointIDs selection preserved, got %#v", matches[0].Selection)
	}

	// collapseToConfigKeys (WorkspaceSDKServiceImpacts' own implementation)
	// must still reduce this down to just the config key.
	collapsed := collapseToConfigKeys(detailed)
	if keys := collapsed[serviceID][versionID]; len(keys) != 1 || keys[0] != "sdk:restricted" {
		t.Fatalf("expected collapseToConfigKeys to project down to [sdk:restricted], got %#v", keys)
	}
}

func TestWorkspaceSDKServiceImpacts_NoConfigStates_EmptyResult(t *testing.T) {
	batchStore := &usageIndexMockBatchStore{}
	configStore := &usageIndexMockConfigStore{}

	impacts, err := WorkspaceSDKServiceImpacts(context.Background(), configStore, batchStore)
	if err != nil {
		t.Fatalf("WorkspaceSDKServiceImpacts: %v", err)
	}
	if len(impacts) != 0 {
		t.Fatalf("expected empty result for no config states, got %#v", impacts)
	}
}

func TestWorkspaceSDKServiceImpacts_ConfigStateWithoutLatestResourceID_Skipped(t *testing.T) {
	batchStore := &usageIndexMockBatchStore{}
	configStore := &usageIndexMockConfigStore{states: []ConfigState{
		{ConfigKey: "sdk:no-resource", ConfigType: ConfigTypeSDK, LatestResourceID: nil},
	}}

	impacts, err := WorkspaceSDKServiceImpacts(context.Background(), configStore, batchStore)
	if err != nil {
		t.Fatalf("WorkspaceSDKServiceImpacts: %v", err)
	}
	if len(impacts) != 0 {
		t.Fatalf("expected a config state with no LatestResourceID to be skipped, got %#v", impacts)
	}
}
