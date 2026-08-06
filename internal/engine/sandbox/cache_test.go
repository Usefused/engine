package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type mockCacheDB struct {
	store.Store
	scopeData         []byte
	scopeVersion      int
	scopeCount        int
	activatedVersion  string
	activatedErr      error
	activatedCalls    int
	contractMetadata  *fusedobject.ServiceMetadata
	contractEndpoints []fusedobject.Endpoint
	contractErr       error
	contractMetaCalls int
	contractNameCalls int
	contractIDCalls   int
	contractListCalls int
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
	if m.contractMetadata == nil {
		return nil, store.ErrServiceContractSnapshotNotFound
	}
	return m.contractMetadata, nil
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
			"endpoint_ids":       []string{uuid.New().String()},
		},
	}
	scopeData, _ := json.Marshal(scopeSelections)

	db := &mockCacheDB{
		scopeData: scopeData,
	}
	rc := &mockRegistryClient{fusedObj: obj}

	cache := NewLocalObjectCache(db, rc)

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
	if db.scopeCount != 1 || rc.fetchCount != 1 {
		t.Errorf("expected 1 fetch from DB for scope and registry for object")
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
	if db.scopeCount != 1 || rc.fetchCount != 1 {
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
			"service_id":   mockID.String(),
			"endpoint_ids": []string{uuid.New().String()},
		},
	})
	db := &mockCacheDB{
		scopeData:    scopeData,
		activatedErr: errors.New("no activated version"),
	}
	rc := &mockRegistryClient{fusedObj: &fusedobject.ServiceMetadata{ID: mockID, Name: "Stripe"}}
	cache := NewLocalObjectCache(db, rc)

	err := cache.ConnectSDK(context.Background(), appID.String())
	if err == nil {
		t.Fatal("expected missing activation version to fail SDK connect")
	}
	if rc.fetchCount != 0 {
		t.Errorf("expected no Registry fetch without a pinned version, got %d", rc.fetchCount)
	}
}

func TestLocalObjectCache_ConnectSDKRequiresSelectionServiceVersionID(t *testing.T) {
	serviceID := uuid.New()
	appID := uuid.New()
	scopeData, _ := json.Marshal([]models.SDKSelection{{
		ServiceID:   serviceID,
		EndpointIDs: []uuid.UUID{uuid.New()},
	}})
	db := &mockCacheDB{
		scopeData:        scopeData,
		activatedVersion: "workspace-latest-public-version",
	}
	rc := &mockRegistryClient{fusedObj: &fusedobject.ServiceMetadata{ID: serviceID, Name: "Stripe"}}
	cache := NewLocalObjectCache(db, rc)

	err := cache.ConnectSDK(context.Background(), appID.String())
	if err == nil {
		t.Fatal("expected unpinned SDK scope to fail")
	}
	if rc.fetchCount != 0 {
		t.Errorf("expected no Registry fetch for unpinned SDK scope, got %d", rc.fetchCount)
	}
	if db.activatedCalls != 0 {
		t.Errorf("runtime SDK scope resolution must not read workspace latest-version state, got %d calls", db.activatedCalls)
	}
}

func TestLocalObjectCache_ConnectSDKUsesSelectionServiceVersionID(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	appID := uuid.New()
	scopeData, _ := json.Marshal([]models.SDKSelection{{
		ServiceID:        serviceID,
		ServiceVersionID: serviceVersionID,
		EndpointIDs:      []uuid.UUID{uuid.New()},
	}})
	db := &mockCacheDB{
		scopeData:        scopeData,
		activatedVersion: "workspace-latest-public-version",
	}
	rc := &mockRegistryClient{fusedObj: &fusedobject.ServiceMetadata{ID: serviceID, Name: "Stripe"}}
	cache := NewLocalObjectCache(db, rc)

	if err := cache.ConnectSDK(context.Background(), appID.String()); err != nil {
		t.Fatalf("ConnectSDK: %v", err)
	}
	if len(rc.fetchedVersions) != 1 || rc.fetchedVersions[0] != serviceVersionID.String() {
		t.Fatalf("expected Registry metadata fetch by service_version_id %s, got %#v", serviceVersionID, rc.fetchedVersions)
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
	rc := &mockRegistryClient{fusedObj: &fusedobject.ServiceMetadata{ID: serviceID, Name: "RegistryService"}}
	cache := NewLocalObjectCache(db, rc)

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
	if rc.fetchCount != 0 || rc.endpointFetchCount != 0 || rc.endpointByNameCount != 0 {
		t.Fatalf("snapshot-backed connect must not call registry, got metadata=%d batched=%d byName=%d", rc.fetchCount, rc.endpointFetchCount, rc.endpointByNameCount)
	}
	if db.contractMetaCalls != 1 || db.contractNameCalls != 1 {
		t.Fatalf("expected one snapshot metadata and one batched-name lookup, got metadata=%d names=%d", db.contractMetaCalls, db.contractNameCalls)
	}
}

func TestLocalObjectCache_GetEndpointDoesNotFallbackWhenSnapshotExists(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	appID := uuid.New()
	scopeData, _ := json.Marshal([]models.SDKSelection{{
		ServiceID:        serviceID,
		ServiceVersionID: serviceVersionID,
		EndpointIDs:      []uuid.UUID{uuid.New()},
	}})
	db := &mockCacheDB{
		scopeData:        scopeData,
		contractMetadata: &fusedobject.ServiceMetadata{ID: serviceID, Name: "SnapshotService"},
	}
	rc := &mockRegistryClient{fusedObj: &fusedobject.ServiceMetadata{ID: serviceID, Name: "RegistryService"}}
	cache := NewLocalObjectCache(db, rc)

	if err := cache.ConnectSDK(context.Background(), appID.String()); err != nil {
		t.Fatalf("ConnectSDK: %v", err)
	}
	_, err := cache.GetEndpoint(context.Background(), appID.String(), serviceID.String(), "registryOnlyOperation")
	if !errors.Is(err, store.ErrServiceContractEndpointNotFound) {
		t.Fatalf("expected local snapshot endpoint miss, got %v", err)
	}
	if rc.endpointByNameCount != 0 {
		t.Fatalf("existing snapshot endpoint miss must not drift to registry, got %d registry calls", rc.endpointByNameCount)
	}
}

func TestLocalObjectCache_ConnectSDKRejectsUnsupportedScopeSchemaVersion(t *testing.T) {
	appID := uuid.New()
	db := &mockCacheDB{
		scopeData: []byte(`[]`),
	}
	c := NewLocalObjectCache(db, &mockRegistryClient{})
	db.scopeVersion = models.AppScopeSchemaVersion + 1
	if err := c.ConnectSDK(context.Background(), appID.String()); err == nil {
		t.Fatal("expected unsupported scope schema version to fail")
	}
}

// TestLocalObjectCache_EndpointsPrefetchedAtConnect verifies the pre-warm path:
// when OperationNames is set in the SDK scope, ConnectSDK must call
// FetchEndpointsByNames once so that the first GetEndpoint call for each
// operation is served from the in-memory cache — no additional Registry fetch.
func TestLocalObjectCache_EndpointsPrefetchedAtConnect(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	appID := uuid.New()

	scopeData, _ := json.Marshal([]models.SDKSelection{{
		ServiceID:        serviceID,
		ServiceVersionID: serviceVersionID,
		EndpointIDs:      []uuid.UUID{uuid.New()},
		OperationNames:   []string{"listUsers", "getUser"},
	}})
	db := &mockCacheDB{scopeData: scopeData}
	rc := &mockRegistryClient{fusedObj: &fusedobject.ServiceMetadata{ID: serviceID, Name: "TestService"}}
	cache := NewLocalObjectCache(db, rc)

	if err := cache.ConnectSDK(context.Background(), appID.String()); err != nil {
		t.Fatalf("ConnectSDK: %v", err)
	}

	// Exactly one FetchEndpointsByNames call should have been made at connect —
	// covering both operation names in one Registry round trip.
	if rc.endpointFetchCount != 1 {
		t.Errorf("expected 1 FetchEndpointsByNames call at connect, got %d", rc.endpointFetchCount)
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
	if rc.endpointFetchCount != 1 {
		t.Errorf("GetEndpoint after pre-warm must be a cache hit; expected endpointFetchCount=1, got %d", rc.endpointFetchCount)
	}
}

// TestLocalObjectCache_EndpointsNotPrefetchedWhenOperationNamesEmpty confirms
// that a scope with no OperationNames (SelectAll=true, or an older scope) does
// NOT trigger FetchEndpointsByNames at connect — lazy fetch still applies.
func TestLocalObjectCache_EndpointsNotPrefetchedWhenOperationNamesEmpty(t *testing.T) {
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	appID := uuid.New()

	scopeData, _ := json.Marshal([]models.SDKSelection{{
		ServiceID:        serviceID,
		ServiceVersionID: serviceVersionID,
		EndpointIDs:      []uuid.UUID{uuid.New()},
		// OperationNames intentionally omitted — simulates SelectAll scope.
	}})
	db := &mockCacheDB{scopeData: scopeData}
	rc := &mockRegistryClient{fusedObj: &fusedobject.ServiceMetadata{ID: serviceID, Name: "TestService"}}
	cache := NewLocalObjectCache(db, rc)

	if err := cache.ConnectSDK(context.Background(), appID.String()); err != nil {
		t.Fatalf("ConnectSDK: %v", err)
	}
	if rc.endpointFetchCount != 0 {
		t.Errorf("expected no endpoint pre-warm for empty OperationNames, got %d", rc.endpointFetchCount)
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
