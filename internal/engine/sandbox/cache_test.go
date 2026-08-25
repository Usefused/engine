package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type mockCacheDB struct {
	store.Store
	scopeData          []byte
	scopeVersion       int
	scopeCount         int
	activatedVersion   string
	activatedErr       error
	activatedCalls     int
	contractMetadata   *fusedobject.ServiceMetadata
	contractEndpoints  []fusedobject.Endpoint
	contractErr        error
	contractBatchErr   error
	policyBatchErr     error
	contractMetaCalls  int
	metadataBatchCalls int
	metadataBatchRefs  int
	policyBatchCalls   int
	policyBatchRefs    int
	policyOverrides    map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride
	contractNameCalls  int
	contractIDCalls    int
	contractListCalls  int
	contractBatchCalls int
	unifiedDescriptors *models.SDKUnifiedOperationDescriptors
	unifiedErr         error
	unifiedCalls       int
}

// GetMCPUnifiedOperationDescriptors supplies an already policy-filtered public
// descriptor so session-fixture tests stay independent from PostgreSQL.
func (m *mockCacheDB) GetMCPUnifiedOperationDescriptors(_ context.Context, _ uuid.UUID, _ bool, _ []string) (*models.SDKUnifiedOperationDescriptors, error) {
	m.unifiedCalls++
	// Test doubles preserve explicit store failures so callers prove they fail
	// closed instead of silently omitting logical operations.
	if m.unifiedErr != nil {
		return nil, m.unifiedErr
	}
	return m.unifiedDescriptors, nil
}

func (m *mockCacheDB) GetSDKAccountID(ctx context.Context, appID uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, nil
}

func (m *mockCacheDB) GetAccountByAPIKey(ctx context.Context, apiKey string) (uuid.UUID, error) {
	return uuid.Nil, nil
}

func (m *mockCacheDB) GetAppRuntime(ctx context.Context, id uuid.UUID) (*store.AppRuntime, error) {
	m.scopeCount++
	version := m.scopeVersion
	if version == 0 {
		version = models.AppScopeSchemaVersion
	}
	return &store.AppRuntime{AppID: id, Selections: m.scopeData, ScopeSchemaVersion: version, Status: "active"}, nil
}

func (m *mockCacheDB) SaveAppRuntime(ctx context.Context, scope store.AppRuntime) error {
	return nil
}

func (m *mockCacheDB) BootstrapWorkspace(ctx context.Context, accountID uuid.UUID, name string) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (m *mockCacheDB) AddWorkspaceServiceVersion(ctx context.Context, serviceID uuid.UUID, serviceSlug string, version string, serviceVersionID uuid.UUID, serviceName string, addedBy uuid.UUID) error {
	return nil
}

func (m *mockCacheDB) EnableWorkspaceServiceVersion(ctx context.Context, serviceID uuid.UUID, version string, serviceVersionID uuid.UUID, enabledBy uuid.UUID) error {
	return nil
}

func (m *mockCacheDB) DisableWorkspaceServiceVersion(ctx context.Context, serviceID uuid.UUID, version string) error {
	return nil
}

func (m *mockCacheDB) ListWorkspaceServiceVersions(ctx context.Context, serviceID uuid.UUID) ([]store.WorkspaceServiceVersion, error) {
	return nil, nil
}

func (m *mockCacheDB) ListWorkspaceServiceVersionsForServices(ctx context.Context, serviceIDs []uuid.UUID) (map[uuid.UUID][]store.WorkspaceServiceVersion, error) {
	return map[uuid.UUID][]store.WorkspaceServiceVersion{}, nil
}

func (m *mockCacheDB) ListWorkspaceServices(ctx context.Context, names []string) ([]store.WorkspaceService, error) {
	return nil, nil
}

func (m *mockCacheDB) RemoveWorkspaceService(ctx context.Context, serviceID uuid.UUID) error {
	return nil
}

func (m *mockCacheDB) IsWorkspaceServiceEnabled(ctx context.Context, serviceID uuid.UUID) (bool, error) {
	return false, nil
}

func (m *mockCacheDB) GetWorkspaceWebhookBySlug(ctx context.Context, slug string) (*store.WorkspaceWebhook, error) {
	return nil, store.ErrWorkspaceWebhookNotFound
}

func (m *mockCacheDB) ListWorkspaceWebhooks(ctx context.Context, serviceID uuid.UUID) ([]store.WorkspaceWebhook, error) {
	return nil, nil
}

func (m *mockCacheDB) GetLatestWorkspaceServiceVersion(ctx context.Context, accountID uuid.UUID, serviceID uuid.UUID) (string, error) {
	m.activatedCalls++
	if m.activatedErr != nil {
		return "", m.activatedErr
	}
	if m.activatedVersion != "" {
		return m.activatedVersion, nil
	}
	return "1.0", nil
}

func (m *mockCacheDB) GetLatestWorkspaceServiceVersionByWorkspace(ctx context.Context, serviceID uuid.UUID) (string, error) {
	return m.GetLatestWorkspaceServiceVersion(ctx, uuid.Nil, serviceID)
}

func (m *mockCacheDB) GetServiceContractMetadata(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*fusedobject.ServiceMetadata, error) {
	m.contractMetaCalls++
	if m.contractErr != nil {
		return nil, m.contractErr
	}
	// An absent fixture represents a missing local immutable snapshot.
	if m.contractMetadata == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	if m.contractMetadata.ContractVersion == 0 {
		m.contractMetadata.ExecutionContractEnvelope = fusedobject.EngineExecutionContractSupport()
	}
	return m.contractMetadata, nil
}

// ListServiceContractMetadata models the production set-based snapshot read
// and records one call regardless of the requested app-scope cardinality.
func (m *mockCacheDB) ListServiceContractMetadata(_ context.Context, refs []store.ServiceContractMetadataRef) (map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata, error) {
	m.metadataBatchCalls++
	m.metadataBatchRefs += len(refs)
	// Metadata failures terminate cold prewarm before any cache state commits.
	if m.contractErr != nil {
		return nil, m.contractErr
	}
	// An absent batch fixture models a requested snapshot missing from Engine.
	if m.contractMetadata == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	result := make(map[store.ServiceContractMetadataRef]*fusedobject.ServiceMetadata, len(refs))
	for _, ref := range refs {
		metadata := *m.contractMetadata
		// Fixtures without an explicit envelope represent current compatible
		// contracts, matching the scalar mock behavior above.
		if metadata.ContractVersion == 0 {
			metadata.ExecutionContractEnvelope = fusedobject.EngineExecutionContractSupport()
		}
		metadata.ID = ref.ServiceID
		metadata.ServiceVersionID = ref.ServiceVersionID
		result[ref] = &metadata
	}
	return result, nil
}

// GetEffectiveWorkspaceExecutionPolicyOverrides models the second cold-cache
// batch and returns no local overlays unless a test supplies an error.
func (m *mockCacheDB) GetEffectiveWorkspaceExecutionPolicyOverrides(_ context.Context, refs []store.WorkspaceExecutionPolicyRef) (map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride, error) {
	m.policyBatchCalls++
	m.policyBatchRefs += len(refs)
	// Policy lookup errors are soft failures, preserving immutable metadata.
	if m.policyBatchErr != nil {
		return nil, m.policyBatchErr
	}
	return m.policyOverrides, nil
}

func (m *mockCacheDB) UpsertServiceContractSnapshot(ctx context.Context, snapshot store.ServiceContractSnapshot) (*store.ServiceContractSnapshot, error) {
	return &snapshot, nil
}

func (m *mockCacheDB) GetServiceContractEndpointByName(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointName string) (*fusedobject.Endpoint, error) {
	m.contractNameCalls++
	if m.contractErr != nil {
		return nil, m.contractErr
	}
	for i := range m.contractEndpoints {
		if m.contractEndpoints[i].Name == endpointName {
			return &m.contractEndpoints[i], nil
		}
	}
	if m.contractMetadata == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	return nil, store.ErrServiceContractEndpointNotFound
}

func (m *mockCacheDB) ListServiceContractEndpointsByNames(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error) {
	m.contractNameCalls++
	if m.contractErr != nil {
		return nil, m.contractErr
	}
	// Endpoint rows cannot exist without their parent snapshot fixture.
	if m.contractMetadata == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	wanted := map[string]struct{}{}
	for _, name := range endpointNames {
		wanted[name] = struct{}{}
	}
	return matchingContractEndpoints(m.contractEndpoints, func(ep fusedobject.Endpoint) bool {
		_, ok := wanted[ep.Name]
		return ok
	}), nil
}

// ListServiceContractEndpointsForSelections models the exact set-based endpoint
// read and records one call for the complete app scope.
func (m *mockCacheDB) ListServiceContractEndpointsForSelections(ctx context.Context, selections []store.ServiceContractEndpointSelection, endpointNames []string) ([]store.ServiceContractEndpointMatch, error) {
	m.contractBatchCalls++
	// A dedicated endpoint failure lets rollback tests prove earlier successful
	// metadata and policy reads were not committed.
	if m.contractBatchErr != nil {
		return nil, m.contractBatchErr
	}
	// Shared fixture errors still model failures common to every contract read.
	if m.contractErr != nil {
		return nil, m.contractErr
	}
	// Endpoint rows cannot exist without their parent snapshot fixture.
	if m.contractMetadata == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	names := make(map[string]struct{}, len(endpointNames))
	for _, name := range endpointNames {
		names[name] = struct{}{}
	}
	var matches []store.ServiceContractEndpointMatch
	for _, selection := range selections {
		ids := make(map[uuid.UUID]struct{}, len(selection.EndpointIDs))
		for _, id := range selection.EndpointIDs {
			ids[id] = struct{}{}
		}
		for _, endpoint := range m.contractEndpoints {
			if mockSelectionAllowsEndpoint(selection, names, ids, endpoint) {
				matches = append(matches, store.ServiceContractEndpointMatch{SelectionIndex: selection.SelectionIndex, Endpoint: endpoint})
			}
		}
	}
	return matches, nil
}

// mockSelectionAllowsEndpoint mirrors the SQL intersection used by the batch
// store without replacing production query coverage.
func mockSelectionAllowsEndpoint(selection store.ServiceContractEndpointSelection, names map[string]struct{}, ids map[uuid.UUID]struct{}, endpoint fusedobject.Endpoint) bool {
	// A global caller allowlist narrows every selection before its local scope.
	if _, allowed := names[endpoint.Name]; len(names) > 0 && !allowed {
		return false
	}
	// Per-selection names keep overlapping service catalogs isolated inside a
	// single batch, mirroring the SQL endpoint_names predicate.
	if len(selection.EndpointNames) > 0 && !slices.Contains(selection.EndpointNames, endpoint.Name) {
		return false
	}
	_, selected := ids[endpoint.ID]
	return selection.SelectAll || selected
}

func (m *mockCacheDB) ListServiceContractEndpointsByIDs(ctx context.Context, serviceID, serviceVersionID uuid.UUID, endpointIDs []uuid.UUID) ([]fusedobject.Endpoint, error) {
	m.contractIDCalls++
	if m.contractErr != nil {
		return nil, m.contractErr
	}
	if m.contractMetadata == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	wanted := map[uuid.UUID]struct{}{}
	for _, id := range endpointIDs {
		wanted[id] = struct{}{}
	}
	return matchingContractEndpoints(m.contractEndpoints, func(ep fusedobject.Endpoint) bool {
		_, ok := wanted[ep.ID]
		return ok
	}), nil
}

func (m *mockCacheDB) ListServiceContractOperations(ctx context.Context, serviceID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error) {
	m.contractListCalls++
	if m.contractErr != nil {
		return nil, m.contractErr
	}
	if m.contractMetadata == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	return append([]fusedobject.Endpoint(nil), m.contractEndpoints...), nil
}

func matchingContractEndpoints(endpoints []fusedobject.Endpoint, keep func(fusedobject.Endpoint) bool) []fusedobject.Endpoint {
	matched := make([]fusedobject.Endpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if keep(endpoint) {
			matched = append(matched, endpoint)
		}
	}
	return matched
}

func (m *mockCacheDB) GetWorkspaceIDForAccount(ctx context.Context, accountID uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, nil
}

func TestLocalObjectCache_Refcounting(t *testing.T) {
	mockID := uuid.New()
	appID := uuid.New()

	obj := &fusedobject.ServiceMetadata{
		ID:   mockID,
		Name: "Stripe",
	}

	scopeSelections := []map[string]interface{}{
		{
			"service_id":         mockID.String(),
			"service_version_id": "00000000-0000-0000-0000-000000000101",
			"schema_version":     models.AppSelectionSchemaVersion,
			"endpoint_ids":       []string{uuid.New().String()},
		},
	}
	scopeData, _ := json.Marshal(scopeSelections)

	db := &mockCacheDB{
		scopeData: scopeData, contractMetadata: obj,
	}
	cache := NewLocalObjectCache(db)

	ctx := context.Background()

	// 1. Connect SDK (Conn 1)
	err := cache.ConnectSDK(ctx, appID.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cache.scopeRefCounts[appID.String()] != 1 {
		t.Errorf("expected scope refcount 1, got %d", cache.scopeRefCounts[appID.String()])
	}
	if cache.objectRefCounts[mockID.String()+":"+"00000000-0000-0000-0000-000000000101"] != 1 {
		t.Errorf("expected object refcount 1, got %d", cache.objectRefCounts[mockID.String()+":"+"00000000-0000-0000-0000-000000000101"])
	}
	if db.scopeCount != 1 || db.metadataBatchCalls != 1 {
		t.Errorf("expected one scope and one snapshot fetch")
	}

	// 2. Connect SDK again (Conn 2)
	err = cache.ConnectSDK(ctx, appID.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cache.scopeRefCounts[appID.String()] != 2 {
		t.Errorf("expected scope refcount 2, got %d", cache.scopeRefCounts[appID.String()])
	}
	if cache.objectRefCounts[mockID.String()+":"+"00000000-0000-0000-0000-000000000101"] != 2 {
		t.Errorf("expected object refcount 2, got %d", cache.objectRefCounts[mockID.String()+":"+"00000000-0000-0000-0000-000000000101"])
	}
	if db.scopeCount != 1 || db.metadataBatchCalls != 1 {
		t.Errorf("expected 0 additional fetches, it should reuse cache")
	}

	// 3. Disconnect SDK (Conn 1)
	cache.DisconnectSDK(appID.String())
	if cache.scopeRefCounts[appID.String()] != 1 {
		t.Errorf("expected scope refcount 1, got %d", cache.scopeRefCounts[appID.String()])
	}

	// Ensure object is still in cache
	cachedObj, err := cache.GetOrFetchServiceMetadata(ctx, appID.String(), mockID.String())
	if err != nil || cachedObj == nil {
		t.Errorf("object should still be in cache")
	}

	// 4. Disconnect SDK (Conn 2) - should evict
	cache.DisconnectSDK(appID.String())
	if cache.scopeRefCounts[appID.String()] != 0 {
		t.Errorf("expected scope refcount 0")
	}
	if cache.objectRefCounts[mockID.String()+":"+"00000000-0000-0000-0000-000000000101"] != 0 {
		t.Errorf("expected object refcount 0")
	}

	// Ensure object is evicted
	_, err = cache.GetOrFetchServiceMetadata(ctx, appID.String(), mockID.String())
	if err == nil {
		t.Errorf("object should be evicted")
	}
}

func TestLocalObjectCache_ConnectSDKRequiresActivatedVersion(t *testing.T) {
	mockID := uuid.New()
	appID := uuid.New()
	scopeData, _ := json.Marshal([]map[string]interface{}{
		{
			"service_id":     mockID.String(),
			"schema_version": models.AppSelectionSchemaVersion,
			"endpoint_ids":   []string{uuid.New().String()},
		},
	})
	db := &mockCacheDB{
		scopeData:    scopeData,
		activatedErr: errors.New("no activated version"),
	}
	cache := NewLocalObjectCache(db)

	err := cache.ConnectSDK(context.Background(), appID.String())
	if err == nil {
		t.Fatal("expected missing activation version to fail SDK connect")
	}
}

func TestLocalObjectCache_ConnectSDKRequiresSelectionServiceVersionID(t *testing.T) {
	serviceID := uuid.New()
	appID := uuid.New()
	scopeData, _ := json.Marshal([]models.SDKSelection{{
		ServiceID:     serviceID,
		SchemaVersion: models.AppSelectionSchemaVersion,
		EndpointIDs:   []uuid.UUID{uuid.New()},
	}})
	db := &mockCacheDB{
		scopeData:        scopeData,
		activatedVersion: "workspace-latest-public-version",
	}
	cache := NewLocalObjectCache(db)

	err := cache.ConnectSDK(context.Background(), appID.String())
	if err == nil {
		t.Fatal("expected unpinned SDK scope to fail")
	}
	if db.activatedCalls != 0 {
		t.Errorf("runtime SDK scope resolution must not read workspace latest-version state, got %d calls", db.activatedCalls)
	}
}

func TestLocalObjectCache_ConnectSDKUsesSelectionServiceVersionSnapshot(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	appID := uuid.New()
	scopeData, _ := json.Marshal([]models.SDKSelection{{
		ServiceID:        serviceID,
		ServiceVersionID: serviceVersionID,
		SchemaVersion:    models.AppSelectionSchemaVersion,
		EndpointIDs:      []uuid.UUID{uuid.New()},
	}})
	db := &mockCacheDB{
		scopeData:        scopeData,
		activatedVersion: "workspace-latest-public-version",
		contractMetadata: &fusedobject.ServiceMetadata{ID: serviceID, Name: "SnapshotService"},
	}
	cache := NewLocalObjectCache(db)

	if err := cache.ConnectSDK(context.Background(), appID.String()); err != nil {
		t.Fatalf("ConnectSDK: %v", err)
	}
	if db.metadataBatchCalls != 1 {
		t.Fatalf("expected one local snapshot batch, got %d", db.metadataBatchCalls)
	}
	if _, err := cache.GetOrFetchServiceMetadata(context.Background(), appID.String(), serviceID.String()); err != nil {
		t.Fatalf("expected metadata cached under service_version_id identity: %v", err)
	}
	if _, ok := cache.serviceMetadataCache[serviceID.String()+":workspace-latest-public-version"]; ok {
		t.Fatal("pinned SDK selection must not cache metadata under the workspace latest public version")
	}
}

func TestLocalObjectCache_ConnectSDKUsesServiceContractSnapshot(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	endpointID := uuid.New()
	appID := uuid.New()
	scopeData, _ := json.Marshal([]models.SDKSelection{{
		ServiceID:        serviceID,
		ServiceVersionID: serviceVersionID,
		SchemaVersion:    models.AppSelectionSchemaVersion,
		EndpointIDs:      []uuid.UUID{endpointID},
		OperationNames:   []string{"listUsers"},
	}})
	db := &mockCacheDB{
		scopeData:        scopeData,
		contractMetadata: &fusedobject.ServiceMetadata{ID: serviceID, Name: "SnapshotService"},
		contractEndpoints: []fusedobject.Endpoint{
			{ID: endpointID, Name: "listUsers", Method: "GET", Path: "/users"},
		},
	}
	cache := NewLocalObjectCache(db)

	if err := cache.ConnectSDK(context.Background(), appID.String()); err != nil {
		t.Fatalf("ConnectSDK: %v", err)
	}
	got, err := cache.GetOrFetchServiceMetadata(context.Background(), appID.String(), serviceID.String())
	if err != nil {
		t.Fatalf("GetOrFetchServiceMetadata: %v", err)
	}
	if got.Name != "SnapshotService" {
		t.Fatalf("expected snapshot metadata, got %#v", got)
	}
	endpoint, err := cache.GetEndpoint(context.Background(), appID.String(), serviceID.String(), "listUsers")
	if err != nil {
		t.Fatalf("GetEndpoint: %v", err)
	}
	if endpoint.ID != endpointID {
		t.Fatalf("expected snapshot endpoint %s, got %#v", endpointID, endpoint)
	}
	if db.metadataBatchCalls != 1 || db.contractBatchCalls != 1 {
		t.Fatalf("expected one metadata and one endpoint batch, got metadata=%d endpoints=%d", db.metadataBatchCalls, db.contractBatchCalls)
	}
}

func TestLocalObjectCache_GetEndpointDoesNotFallbackWhenSnapshotExists(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	appID := uuid.New()
	scopeData, _ := json.Marshal([]models.SDKSelection{{
		ServiceID:        serviceID,
		ServiceVersionID: serviceVersionID,
		SchemaVersion:    models.AppSelectionSchemaVersion,
		EndpointIDs:      []uuid.UUID{uuid.New()},
	}})
	db := &mockCacheDB{
		scopeData:        scopeData,
		contractMetadata: &fusedobject.ServiceMetadata{ID: serviceID, Name: "SnapshotService"},
	}
	cache := NewLocalObjectCache(db)

	if err := cache.ConnectSDK(context.Background(), appID.String()); err != nil {
		t.Fatalf("ConnectSDK: %v", err)
	}
	_, err := cache.GetEndpoint(context.Background(), appID.String(), serviceID.String(), "registryOnlyOperation")
	if !errors.Is(err, store.ErrServiceContractEndpointNotFound) {
		t.Fatalf("expected local snapshot endpoint miss, got %v", err)
	}
}

func TestLocalObjectCache_MissingSnapshotNeverFallsBackToRegistry(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	cache := NewLocalObjectCache(&mockCacheDB{})

	if _, _, err := cache.fetchServiceMetadata(context.Background(), serviceID, versionID.String()); !errors.Is(err, store.ErrServiceContractSnapshotNotFound) {
		t.Fatalf("metadata snapshot miss = %v", err)
	}
	if _, _, err := cache.fetchEndpointByName(context.Background(), serviceID, versionID, "listUsers"); !errors.Is(err, store.ErrServiceContractSnapshotNotFound) {
		t.Fatalf("endpoint snapshot miss = %v", err)
	}
	if _, _, err := cache.fetchEndpointsByNames(context.Background(), serviceID, versionID, []string{"listUsers"}); !errors.Is(err, store.ErrServiceContractSnapshotNotFound) {
		t.Fatalf("endpoint batch snapshot miss = %v", err)
	}
}

func TestLocalObjectCache_ConnectFailsWhenNamedEndpointSnapshotIsIncomplete(t *testing.T) {
	serviceID, versionID, appID := uuid.New(), uuid.New(), uuid.New()
	scopeData, _ := json.Marshal([]models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: versionID, SchemaVersion: models.AppSelectionSchemaVersion, OperationNames: []string{"listUsers"},
	}})
	cache := NewLocalObjectCache(&mockCacheDB{
		scopeData: scopeData, contractMetadata: &fusedobject.ServiceMetadata{ID: serviceID, Name: "SnapshotService"},
	})

	err := cache.ConnectSDK(context.Background(), appID.String())
	if !errors.Is(err, store.ErrServiceContractEndpointNotFound) {
		t.Fatalf("incomplete endpoint snapshot error = %v", err)
	}
	if cache.sdkVersions[appID.String()] != nil || len(cache.serviceMetadataCache) != 0 || len(cache.objectRefCounts) != 0 {
		t.Fatalf("failed connect retained partial cache state: versions=%#v metadata=%d refs=%#v", cache.sdkVersions[appID.String()], len(cache.serviceMetadataCache), cache.objectRefCounts)
	}
}

func TestLocalObjectCacheRejectsIncompatibleCachedContractBeforeDispatch(t *testing.T) {
	serviceID, versionID, appID := uuid.New(), uuid.New(), uuid.New()
	cache := NewLocalObjectCache(&mockCacheDB{})
	cache.sdkVersions[appID.String()] = map[string]string{serviceID.String(): versionID.String()}
	cache.serviceMetadataCache[serviceID.String()+":"+versionID.String()] = &fusedobject.ServiceMetadata{
		ExecutionContractEnvelope: fusedobject.ExecutionContractEnvelope{
			ContractVersion:      fusedobject.CurrentExecutionContractVersion,
			RequiredCapabilities: []string{"http.future.v1"},
		},
	}

	_, err := cache.GetOrFetchServiceMetadata(context.Background(), appID.String(), serviceID.String())
	if err == nil || !strings.Contains(err.Error(), fusedobject.ExecutionCapabilityRequiredCode) {
		t.Fatalf("cached compatibility error = %v", err)
	}
}

func TestLocalObjectCache_ConnectSDKRejectsUnsupportedScopeSchemaVersion(t *testing.T) {
	appID := uuid.New()
	db := &mockCacheDB{
		scopeData: []byte(`[]`),
	}
	c := NewLocalObjectCache(db)
	db.scopeVersion = models.AppScopeSchemaVersion + 1
	if err := c.ConnectSDK(context.Background(), appID.String()); err == nil {
		t.Fatal("expected unsupported scope schema version to fail")
	}
}

func TestLocalObjectCacheConnectRejectsUnsupportedSelectionSchemaBeforeSnapshotReads(t *testing.T) {
	serviceID, serviceVersionID := uuid.New(), uuid.New()
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "missing", payload: []byte(`[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `"}]`)},
		{name: "old", payload: []byte(`[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","schema_version":2}]`)},
		{name: "future", payload: []byte(`[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","schema_version":4}]`)},
		{name: "removed field", payload: []byte(`[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","definition_schema_version":3}]`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := &mockCacheDB{scopeData: test.payload}
			cache := NewLocalObjectCache(db)
			if err := cache.ConnectSDK(context.Background(), uuid.NewString()); err == nil {
				t.Fatal("expected unsupported selection schema to fail")
			}
			if db.metadataBatchCalls != 0 || db.contractBatchCalls != 0 {
				t.Fatalf("invalid selection reached snapshot reads: metadata=%d endpoints=%d", db.metadataBatchCalls, db.contractBatchCalls)
			}
		})
	}
}

// TestLocalObjectCache_EndpointsPrefetchedAtConnect verifies the pre-warm path:
// when OperationNames is set in the SDK scope, ConnectSDK must include it in
// one set-based snapshot read so the first dispatch uses the in-memory cache.
func TestLocalObjectCache_EndpointsPrefetchedAtConnect(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	appID := uuid.New()

	scopeData, _ := json.Marshal([]models.SDKSelection{{
		ServiceID:        serviceID,
		ServiceVersionID: serviceVersionID,
		SchemaVersion:    models.AppSelectionSchemaVersion,
		EndpointIDs:      []uuid.UUID{uuid.New()},
		OperationNames:   []string{"listUsers", "getUser"},
	}})
	db := &mockCacheDB{
		scopeData:         scopeData,
		contractMetadata:  &fusedobject.ServiceMetadata{ID: serviceID, Name: "TestService"},
		contractEndpoints: []fusedobject.Endpoint{{Name: "listUsers"}, {Name: "getUser"}},
	}
	cache := NewLocalObjectCache(db)

	if err := cache.ConnectSDK(context.Background(), appID.String()); err != nil {
		t.Fatalf("ConnectSDK: %v", err)
	}

	// Exactly one snapshot batch should cover both names without consulting the
	// Registry on the execution path.
	if db.contractBatchCalls != 1 {
		t.Errorf("expected one snapshot batch, got %d", db.contractBatchCalls)
	}

	// GetEndpoint for a pre-warmed name must be a cache hit — endpointFetchCount
	// must stay at 1 (FetchEndpointByName is not called).
	ep, err := cache.GetEndpoint(context.Background(), appID.String(), serviceID.String(), "listUsers")
	if err != nil {
		t.Fatalf("GetEndpoint after pre-warm: %v", err)
	}
	if ep.Name != "listUsers" {
		t.Errorf("expected endpoint name 'listUsers', got %q", ep.Name)
	}
	if db.contractNameCalls != 0 || db.contractBatchCalls != 1 {
		t.Errorf("GetEndpoint after pre-warm must be a cache hit; scalar=%d batch=%d", db.contractNameCalls, db.contractBatchCalls)
	}
}

// TestLocalObjectCacheColdPrewarmUsesConstantQueryCount proves app-scope size
// changes row cardinality but not metadata, policy, or endpoint round trips.
func TestLocalObjectCacheColdPrewarmUsesConstantQueryCount(t *testing.T) {
	for _, selectionCount := range []int{1, 100, 1000} {
		t.Run(fmt.Sprintf("selections_%d", selectionCount), func(t *testing.T) {
			selections := make([]models.SDKSelection, selectionCount)
			for index := range selections {
				selections[index] = models.SDKSelection{
					ServiceID: uuid.New(), ServiceVersionID: uuid.New(), OperationNames: []string{"listItems"},
				}
			}
			database := &mockCacheDB{
				contractMetadata:  &fusedobject.ServiceMetadata{Name: "BatchService"},
				contractEndpoints: []fusedobject.Endpoint{{Name: "listItems"}},
			}
			cache := NewLocalObjectCache(database)
			if err := cache.cacheSDKSelections(context.Background(), uuid.NewString(), selections); err != nil {
				t.Fatalf("cacheSDKSelections: %v", err)
			}
			if database.metadataBatchCalls != 1 || database.policyBatchCalls != 1 || database.contractBatchCalls != 1 {
				t.Fatalf("batch calls metadata=%d policy=%d endpoints=%d", database.metadataBatchCalls, database.policyBatchCalls, database.contractBatchCalls)
			}
			if database.contractMetaCalls != 0 || database.contractNameCalls != 0 {
				t.Fatalf("scalar calls metadata=%d endpoints=%d", database.contractMetaCalls, database.contractNameCalls)
			}
		})
	}
}

// TestLocalObjectCacheColdPrewarmReusesWarmMetadata proves a second app can
// retain shared metadata without repeating either metadata or policy reads.
func TestLocalObjectCacheColdPrewarmReusesWarmMetadata(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	cacheKey := serviceID.String() + ":" + versionID.String()
	database := &mockCacheDB{contractMetadata: &fusedobject.ServiceMetadata{Name: "Unused"}}
	cache := NewLocalObjectCache(database)
	cache.serviceMetadataCache[cacheKey] = &fusedobject.ServiceMetadata{Name: "Warm"}
	selection := models.SDKSelection{ServiceID: serviceID, ServiceVersionID: versionID, SelectAll: true}
	if err := cache.cacheSDKSelections(context.Background(), uuid.NewString(), []models.SDKSelection{selection}); err != nil {
		t.Fatalf("cacheSDKSelections: %v", err)
	}
	if database.metadataBatchCalls != 0 || database.policyBatchCalls != 0 || database.contractBatchCalls != 0 {
		t.Fatalf("warm selection queried metadata=%d policy=%d endpoints=%d", database.metadataBatchCalls, database.policyBatchCalls, database.contractBatchCalls)
	}
	if cache.objectRefCounts[cacheKey] != 1 || cache.serviceMetadataCache[cacheKey].Name != "Warm" {
		t.Fatalf("warm entry was not retained: refs=%d metadata=%#v", cache.objectRefCounts[cacheKey], cache.serviceMetadataCache[cacheKey])
	}
}

// TestLocalObjectCacheColdPrewarmDeduplicatesMetadataRefs ensures duplicate
// app selections share batch input while retaining one reference per selection.
func TestLocalObjectCacheColdPrewarmDeduplicatesMetadataRefs(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	selection := models.SDKSelection{ServiceID: serviceID, ServiceVersionID: versionID, SelectAll: true}
	database := &mockCacheDB{contractMetadata: &fusedobject.ServiceMetadata{Name: "Duplicate"}}
	cache := NewLocalObjectCache(database)
	if err := cache.cacheSDKSelections(context.Background(), uuid.NewString(), []models.SDKSelection{selection, selection}); err != nil {
		t.Fatalf("cacheSDKSelections: %v", err)
	}
	cacheKey := serviceID.String() + ":" + versionID.String()
	if database.metadataBatchRefs != 1 || database.policyBatchRefs != 1 {
		t.Fatalf("duplicate batch refs metadata=%d policy=%d", database.metadataBatchRefs, database.policyBatchRefs)
	}
	if cache.objectRefCounts[cacheKey] != 2 || len(cache.serviceMetadataCache) != 1 {
		t.Fatalf("duplicate refcounts=%#v metadata=%d", cache.objectRefCounts, len(cache.serviceMetadataCache))
	}
}

// TestLocalObjectCacheColdPrewarmAppliesBatchedPolicy verifies workspace policy
// rows are merged into staged metadata before the cache commit.
func TestLocalObjectCacheColdPrewarmAppliesBatchedPolicy(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	ref := store.WorkspaceExecutionPolicyRef{ServiceID: serviceID, ServiceVersionID: versionID}
	overriddenBaseURL := "https://workspace.example.com"
	database := &mockCacheDB{
		contractMetadata: &fusedobject.ServiceMetadata{Name: "Policy", BaseURL: "https://provider.example.com"},
		policyOverrides: map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride{
			ref: {BaseURL: &overriddenBaseURL},
		},
	}
	cache := NewLocalObjectCache(database)
	selection := models.SDKSelection{ServiceID: serviceID, ServiceVersionID: versionID, SelectAll: true}
	if err := cache.cacheSDKSelections(context.Background(), uuid.NewString(), []models.SDKSelection{selection}); err != nil {
		t.Fatalf("cacheSDKSelections: %v", err)
	}
	cacheKey := serviceID.String() + ":" + versionID.String()
	if cache.serviceMetadataCache[cacheKey].BaseURL != overriddenBaseURL || len(cache.serviceMetadataCache[cacheKey].Servers) != 0 {
		t.Fatalf("batched policy metadata = %#v", cache.serviceMetadataCache[cacheKey])
	}
}

// TestLocalObjectCacheColdPrewarmPreservesPerSelectionNames verifies the one
// endpoint batch does not populate either service with another selection's API.
func TestLocalObjectCacheColdPrewarmPreservesPerSelectionNames(t *testing.T) {
	firstServiceID, firstVersionID := uuid.New(), uuid.New()
	secondServiceID, secondVersionID := uuid.New(), uuid.New()
	selections := []models.SDKSelection{
		{ServiceID: firstServiceID, ServiceVersionID: firstVersionID, OperationNames: []string{"listFirst"}},
		{ServiceID: secondServiceID, ServiceVersionID: secondVersionID, OperationNames: []string{"listSecond"}},
	}
	database := &mockCacheDB{
		contractMetadata: &fusedobject.ServiceMetadata{Name: "Scoped"},
		contractEndpoints: []fusedobject.Endpoint{
			{Name: "listFirst"}, {Name: "listSecond"},
		},
	}
	cache := NewLocalObjectCache(database)
	if err := cache.cacheSDKSelections(context.Background(), uuid.NewString(), selections); err != nil {
		t.Fatalf("cacheSDKSelections: %v", err)
	}
	firstPrefix := firstServiceID.String() + ":" + firstVersionID.String() + ":"
	secondPrefix := secondServiceID.String() + ":" + secondVersionID.String() + ":"
	if cache.endpointMetadataCache[firstPrefix+"listFirst"] == nil || cache.endpointMetadataCache[firstPrefix+"listSecond"] != nil {
		t.Fatalf("first selection endpoint cache = %#v", cache.endpointMetadataCache)
	}
	if cache.endpointMetadataCache[secondPrefix+"listSecond"] == nil || cache.endpointMetadataCache[secondPrefix+"listFirst"] != nil {
		t.Fatalf("second selection endpoint cache = %#v", cache.endpointMetadataCache)
	}
}

// TestLocalObjectCacheColdPrewarmMetadataFailurePreservesCacheState proves the
// first batch cannot publish versions, metadata, endpoints, or refcounts.
func TestLocalObjectCacheColdPrewarmMetadataFailurePreservesCacheState(t *testing.T) {
	cache := NewLocalObjectCache(&mockCacheDB{contractErr: errors.New("metadata batch unavailable")})
	selection := models.SDKSelection{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SelectAll: true}
	appID := uuid.NewString()
	err := cache.cacheSDKSelections(context.Background(), appID, []models.SDKSelection{selection})
	if err == nil || !strings.Contains(err.Error(), "metadata batch unavailable") {
		t.Fatalf("cacheSDKSelections error = %v", err)
	}
	if cache.sdkVersions[appID] != nil || len(cache.serviceMetadataCache) != 0 || len(cache.objectRefCounts) != 0 || len(cache.endpointMetadataCache) != 0 {
		t.Fatalf("metadata failure changed cache: versions=%#v metadata=%#v refs=%#v endpoints=%#v", cache.sdkVersions[appID], cache.serviceMetadataCache, cache.objectRefCounts, cache.endpointMetadataCache)
	}
}

// TestLocalObjectCacheColdPrewarmFailurePreservesCacheState verifies the
// staged transaction leaves both existing entries and new refcounts untouched.
func TestLocalObjectCacheColdPrewarmFailurePreservesCacheState(t *testing.T) {
	retainedServiceID, retainedVersionID := uuid.New(), uuid.New()
	retainedKey := retainedServiceID.String() + ":" + retainedVersionID.String()
	database := &mockCacheDB{
		contractMetadata: &fusedobject.ServiceMetadata{Name: "BatchService"},
		contractBatchErr: errors.New("endpoint batch unavailable"),
	}
	cache := NewLocalObjectCache(database)
	cache.serviceMetadataCache[retainedKey] = &fusedobject.ServiceMetadata{Name: "Retained"}
	cache.objectRefCounts[retainedKey] = 2
	selections := []models.SDKSelection{{
		ServiceID: uuid.New(), ServiceVersionID: uuid.New(), OperationNames: []string{"listItems"},
	}}
	appID := uuid.NewString()
	err := cache.cacheSDKSelections(context.Background(), appID, selections)
	if err == nil || !strings.Contains(err.Error(), "endpoint batch unavailable") {
		t.Fatalf("cacheSDKSelections error = %v", err)
	}
	if cache.sdkVersions[appID] != nil || len(cache.serviceMetadataCache) != 1 || cache.objectRefCounts[retainedKey] != 2 || len(cache.endpointMetadataCache) != 0 {
		t.Fatalf("failed prewarm changed cache: versions=%#v metadata=%#v refs=%#v endpoints=%#v", cache.sdkVersions[appID], cache.serviceMetadataCache, cache.objectRefCounts, cache.endpointMetadataCache)
	}
}

// TestLocalObjectCache_EndpointsNotPrefetchedWhenOperationNamesEmpty confirms
// that a scope with no OperationNames (SelectAll=true, or an older scope) does
// not trigger a snapshot endpoint batch at connect; lazy fetch still applies.
func TestLocalObjectCache_EndpointsNotPrefetchedWhenOperationNamesEmpty(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	appID := uuid.New()

	scopeData, _ := json.Marshal([]models.SDKSelection{{
		ServiceID:        serviceID,
		ServiceVersionID: serviceVersionID,
		SchemaVersion:    models.AppSelectionSchemaVersion,
		EndpointIDs:      []uuid.UUID{uuid.New()},
		// OperationNames intentionally omitted — simulates SelectAll scope.
	}})
	db := &mockCacheDB{scopeData: scopeData, contractMetadata: &fusedobject.ServiceMetadata{ID: serviceID, Name: "TestService"}}
	cache := NewLocalObjectCache(db)

	if err := cache.ConnectSDK(context.Background(), appID.String()); err != nil {
		t.Fatalf("ConnectSDK: %v", err)
	}
	if db.contractBatchCalls != 0 {
		t.Fatalf("select-all prewarm endpoint batches = %d, want 0", db.contractBatchCalls)
	}
}

func (m *mockCacheDB) BatchCreateWebhookEvents(ctx context.Context, events []models.WebhookEvent) error {
	return nil
}

func (m *mockCacheDB) BatchCreateEngineExecutionEvents(ctx context.Context, events []models.EngineExecutionEvent) error {
	return nil
}

func (m *mockCacheDB) UpsertMCPSession(ctx context.Context, session *models.MCPSession) error {
	return nil
}

func (m *mockCacheDB) GetIdempotentExecution(ctx context.Context, appID uuid.UUID, idempotencyKeyHash, requestBodyHash string) (*models.IdempotentExecution, error) {
	return nil, store.ErrIdempotentExecutionNotFound
}

func (m *mockCacheDB) SaveIdempotentExecution(ctx context.Context, exec *models.IdempotentExecution) error {
	return nil
}
