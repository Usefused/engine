package store

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/shared/cache"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

func TestCachedStoreGetSecretsCachesBatchHitsAndMisses(t *testing.T) {
	bucketID := uuid.New()
	serviceID := uuid.New()
	delegate := &batchSecretDelegate{
		secrets: map[string]WorkspaceSecret{
			"basicAuth_username": {WorkspaceSecretMeta: WorkspaceSecretMeta{BucketID: bucketID, ServiceID: serviceID, KeyName: "basicAuth_username"}},
		},
	}
	cached := &cachedStore{Store: delegate, cache: cache.NewInMemoryCache()}

	first, err := cached.GetSecrets(context.Background(), bucketID, serviceID, []string{"basicAuth_username", "basicAuth_password"})
	if err != nil {
		t.Fatalf("first GetSecrets failed: %v", err)
	}
	second, err := cached.GetSecrets(context.Background(), bucketID, serviceID, []string{"basicAuth_username", "basicAuth_password"})
	if err != nil {
		t.Fatalf("second GetSecrets failed: %v", err)
	}

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("expected one cached hit result each time, got first=%#v second=%#v", first, second)
	}
	if delegate.calls != 1 {
		t.Fatalf("expected one delegate batch call because miss is cached too, got %d", delegate.calls)
	}
	if got := delegate.keys[0]; got != "basicAuth_username,basicAuth_password" {
		t.Fatalf("expected one batch lookup for both basic keys, got %q", got)
	}
}

// TestCachedStoreForwardsProfileCapabilities prevents an interface wrapper
// from silently dropping targeted runtime reads or fixed-query apply writes.
func TestCachedStoreForwardsProfileCapabilities(t *testing.T) {
	delegate := &cachedProfileDelegate{}
	cached := NewCachedStore(delegate, nil)
	bindings, ok := cached.(WorkspaceProfileStore)
	if !ok {
		t.Fatal("cached store does not expose profile binding capability")
	}
	if _, err := bindings.ListWorkspaceBindingsForExecution(context.Background(), uuid.New(), uuid.New(), uuid.New(), "oauth", "getIssue"); err != nil {
		t.Fatalf("forward execution bindings: %v", err)
	}
	batch, ok := cached.(WorkspaceProfileBatchStore)
	if !ok {
		t.Fatal("cached store does not expose profile batch capability")
	}
	if err := batch.ReconcileWorkspaceProfiles(context.Background(), nil, nil); err != nil {
		t.Fatalf("forward profile batch: %v", err)
	}
	status, ok := cached.(WorkspaceServiceVersionStatusStore)
	if !ok {
		t.Fatal("cached store does not expose exact service-version status capability")
	}
	if active, err := status.IsWorkspaceServiceVersionActive(context.Background(), uuid.New(), uuid.New()); err != nil || !active {
		t.Fatalf("forward service-version status: active=%v err=%v", active, err)
	}
	lookup, ok := cached.(WorkspaceServiceVersionLookupStore)
	if !ok {
		t.Fatal("cached store does not expose exact service-version lookup capability")
	}
	if _, err := lookup.GetWorkspaceServiceVersion(context.Background(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("forward service-version lookup: %v", err)
	}
	backfill, ok := cached.(WorkspaceServiceVersionContractBackfillStore)
	if !ok {
		t.Fatal("cached store does not expose missing contract snapshot lookup capability")
	}
	if _, err := backfill.ListWorkspaceServiceVersionsMissingContractSnapshots(context.Background(), 50); err != nil {
		t.Fatalf("forward missing contract snapshot lookup: %v", err)
	}
	snapshots, ok := cached.(ServiceContractSnapshotStore)
	if !ok {
		t.Fatal("cached store does not expose service contract snapshot capability")
	}
	if _, err := snapshots.GetServiceContractMetadata(context.Background(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("forward service contract metadata: %v", err)
	}
	if _, err := snapshots.GetServiceContractEndpointByName(context.Background(), uuid.New(), uuid.New(), "getIssue"); err != nil {
		t.Fatalf("forward service contract endpoint by name: %v", err)
	}
	if _, err := snapshots.ListServiceContractEndpointsByNames(context.Background(), uuid.New(), uuid.New(), []string{"getIssue"}); err != nil {
		t.Fatalf("forward service contract endpoints by names: %v", err)
	}
	if _, err := snapshots.ListServiceContractEndpointsByIDs(context.Background(), uuid.New(), uuid.New(), []uuid.UUID{uuid.New()}); err != nil {
		t.Fatalf("forward service contract endpoints by IDs: %v", err)
	}
	if _, err := snapshots.ListServiceContractOperations(context.Background(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("forward service contract operations: %v", err)
	}
	if delegate.executionCalls != 1 || delegate.batchCalls != 1 || delegate.statusCalls != 1 || delegate.lookupCalls != 1 || delegate.backfillCalls != 1 || delegate.snapshotCalls != 5 {
		t.Fatalf("delegate calls execution=%d batch=%d status=%d lookup=%d backfill=%d snapshots=%d", delegate.executionCalls, delegate.batchCalls, delegate.statusCalls, delegate.lookupCalls, delegate.backfillCalls, delegate.snapshotCalls)
	}
}

// TestCachedStoreForwardsWorkspaceConnectSync prevents the cache wrapper from
// hiding the fixed-query export methods required by workspace sync.
func TestCachedStoreForwardsWorkspaceConnectSync(t *testing.T) {
	delegate := &cachedWorkspaceConnectSyncDelegate{}
	cached := NewCachedStore(delegate, nil)
	syncStore, ok := cached.(WorkspaceConnectSyncStore)
	if !ok {
		t.Fatal("cached store does not expose workspace connect sync capability")
	}
	if _, err := syncStore.ListWorkspaceConnectConfigs(context.Background()); err != nil {
		t.Fatalf("forward workspace connect configs: %v", err)
	}
	if _, err := syncStore.ListWorkspaceConnectProfiles(context.Background()); err != nil {
		t.Fatalf("forward workspace connect profiles: %v", err)
	}
	if delegate.configCalls != 1 || delegate.profileCalls != 1 {
		t.Fatalf("delegate calls configs=%d profiles=%d", delegate.configCalls, delegate.profileCalls)
	}
}

type cachedProfileDelegate struct {
	Store
	WorkspaceProfileStore
	executionCalls int
	batchCalls     int
	statusCalls    int
	lookupCalls    int
	backfillCalls  int
	snapshotCalls  int
}

type cachedWorkspaceConnectSyncDelegate struct {
	Store
	configCalls  int
	profileCalls int
}

// ListWorkspaceConnectConfigs records forwarding of the config half of the
// workspace export without introducing data that could mask call counts.
func (d *cachedWorkspaceConnectSyncDelegate) ListWorkspaceConnectConfigs(context.Context) ([]WorkspaceConnectConfig, error) {
	d.configCalls++
	return nil, nil
}

// ListWorkspaceConnectProfiles records forwarding of the profile half so the
// regression test covers both queries used by one GraphQL resolver.
func (d *cachedWorkspaceConnectSyncDelegate) ListWorkspaceConnectProfiles(context.Context) ([]WorkspaceConnectionProfile, error) {
	d.profileCalls++
	return nil, nil
}

// ListWorkspaceBindingsForExecution records the targeted hot-path forwarding call.
func (d *cachedProfileDelegate) ListWorkspaceBindingsForExecution(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string) ([]WorkspaceConnectionBinding, error) {
	d.executionCalls++
	return nil, nil
}

// ReconcileWorkspaceProfiles records the fixed-query apply forwarding call.
func (d *cachedProfileDelegate) ReconcileWorkspaceProfiles(context.Context, []WorkspaceProfileReplacement, []WorkspaceProfileRef) error {
	d.batchCalls++
	return nil
}

// IsWorkspaceServiceVersionActive records forwarding of the exact activation lookup.
func (d *cachedProfileDelegate) IsWorkspaceServiceVersionActive(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	d.statusCalls++
	return true, nil
}

func (d *cachedProfileDelegate) GetWorkspaceServiceVersion(context.Context, uuid.UUID, uuid.UUID) (*WorkspaceServiceVersion, error) {
	d.lookupCalls++
	return &WorkspaceServiceVersion{}, nil
}

func (d *cachedProfileDelegate) ListWorkspaceServiceVersionsMissingContractSnapshots(context.Context, int) ([]WorkspaceServiceVersion, error) {
	d.backfillCalls++
	return nil, nil
}

func (d *cachedProfileDelegate) UpsertServiceContractSnapshot(context.Context, ServiceContractSnapshot) (*ServiceContractSnapshot, error) {
	d.snapshotCalls++
	return &ServiceContractSnapshot{}, nil
}

func (d *cachedProfileDelegate) GetServiceContractMetadata(context.Context, uuid.UUID, uuid.UUID) (*fusedobject.ServiceMetadata, error) {
	d.snapshotCalls++
	return &fusedobject.ServiceMetadata{}, nil
}

func (d *cachedProfileDelegate) GetServiceContractEndpointByName(context.Context, uuid.UUID, uuid.UUID, string) (*fusedobject.Endpoint, error) {
	d.snapshotCalls++
	return &fusedobject.Endpoint{}, nil
}

func (d *cachedProfileDelegate) ListServiceContractEndpointsByNames(context.Context, uuid.UUID, uuid.UUID, []string) ([]fusedobject.Endpoint, error) {
	d.snapshotCalls++
	return nil, nil
}

func (d *cachedProfileDelegate) ListServiceContractEndpointsByIDs(context.Context, uuid.UUID, uuid.UUID, []uuid.UUID) ([]fusedobject.Endpoint, error) {
	d.snapshotCalls++
	return nil, nil
}

func (d *cachedProfileDelegate) ListServiceContractOperations(context.Context, uuid.UUID, uuid.UUID) ([]fusedobject.Endpoint, error) {
	d.snapshotCalls++
	return nil, nil
}

type batchSecretDelegate struct {
	Store
	secrets map[string]WorkspaceSecret
	keys    []string
	calls   int
}

func (d *batchSecretDelegate) GetSecrets(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) ([]WorkspaceSecret, error) {
	d.calls++
	d.keys = append(d.keys, joinSecretKeys(keyNames))
	var out []WorkspaceSecret
	for _, keyName := range keyNames {
		if secret, ok := d.secrets[keyName]; ok {
			out = append(out, secret)
		}
	}
	return out, nil
}

func joinSecretKeys(keys []string) string {
	out := ""
	for i, key := range keys {
		if i > 0 {
			out += ","
		}
		out += key
	}
	return out
}
