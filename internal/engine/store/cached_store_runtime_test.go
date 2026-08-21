package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/google/uuid"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestCachedStoreAppRuntimeHitReturnsDefensiveCopy(t *testing.T) {
	appID := uuid.New()
	delegate := newRuntimeCacheDelegate(AppRuntime{
		AppID:      appID,
		Version:    "v1",
		Selections: []byte(`[{"service":"linear"}]`),
	})
	cached := NewCachedStore(delegate, nil)

	first, err := cached.GetAppRuntime(context.Background(), appID)
	if err != nil {
		t.Fatalf("first GetAppRuntime: %v", err)
	}
	first.Selections[0] = '!'
	first.Version = "mutated"

	second, err := cached.GetAppRuntime(context.Background(), appID)
	if err != nil {
		t.Fatalf("second GetAppRuntime: %v", err)
	}
	if second.Version != "v1" || string(second.Selections) != `[{"service":"linear"}]` {
		t.Fatalf("cached runtime was mutated through caller copy: %#v", second)
	}
	if calls := delegate.loadCount(); calls != 1 {
		t.Fatalf("delegate loads = %d, want 1", calls)
	}
}

func TestCachedStoreAppRuntimeCoalescesConcurrentMisses(t *testing.T) {
	appID := uuid.New()
	release := make(chan struct{})
	delegate := newRuntimeCacheDelegate(AppRuntime{AppID: appID, Version: "v1"})
	delegate.release = release
	cached := NewCachedStore(delegate, nil)

	const callers = 24
	start := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			_, err := cached.GetAppRuntime(context.Background(), appID)
			results <- err
		}()
	}
	close(start)

	select {
	case <-delegate.started:
	case <-time.After(time.Second):
		t.Fatal("delegate load did not start")
	}
	// Holding the leader briefly makes every contender overlap the same miss;
	// the assertion then proves one database boundary, not scheduler luck.
	time.Sleep(25 * time.Millisecond)
	close(release)
	for range callers {
		if err := <-results; err != nil {
			t.Fatalf("coalesced GetAppRuntime: %v", err)
		}
	}
	if calls := delegate.loadCount(); calls != 1 {
		t.Fatalf("delegate loads = %d, want 1", calls)
	}
}

func TestCachedStoreAppRuntimeAbsoluteTTLRecoversWithoutInvalidation(t *testing.T) {
	appID := uuid.New()
	delegate := newRuntimeCacheDelegate(AppRuntime{AppID: appID, Version: "v1"})
	cached := NewCachedStore(delegate, nil).(*cachedStore)
	cached.runtime.ttl = 40 * time.Millisecond

	if runtime := mustGetRuntime(t, cached, appID); runtime.Version != "v1" {
		t.Fatalf("initial version = %q, want v1", runtime.Version)
	}
	delegate.setRuntime(AppRuntime{AppID: appID, Version: "v2"})

	deadline := time.Now().Add(750 * time.Millisecond)
	for {
		runtime := mustGetRuntime(t, cached, appID)
		if runtime.Version == "v2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hot reads kept stale runtime alive past its absolute TTL")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls := delegate.loadCount(); calls != 2 {
		t.Fatalf("delegate loads = %d, want exactly one TTL fallback reload", calls)
	}
}

func TestCachedStoreGenerationFenceRejectsPreInvalidationLoad(t *testing.T) {
	appID := uuid.New()
	delegate := newRuntimeGenerationFenceDelegate(appID)
	cached := NewCachedStore(delegate, nil).(*cachedStore)
	firstResult := make(chan *AppRuntime, 1)
	firstError := make(chan error, 1)
	go func() {
		runtime, err := cached.GetAppRuntime(context.Background(), appID)
		firstResult <- runtime
		firstError <- err
	}()

	select {
	case <-delegate.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("pre-invalidation load did not start")
	}
	cached.NotifyAppRuntimeChanged(context.Background(), appID)
	if runtime := mustGetRuntime(t, cached, appID); runtime.Version != "v2" {
		t.Fatalf("post-invalidation runtime = %q, want v2", runtime.Version)
	}
	close(delegate.firstRelease)
	if err := <-firstError; err != nil {
		t.Fatalf("pre-invalidation request: %v", err)
	}
	if runtime := <-firstResult; runtime == nil || runtime.Version != "v1" {
		t.Fatalf("pre-invalidation request runtime = %#v, want v1 snapshot", runtime)
	}
	if runtime := mustGetRuntime(t, cached, appID); runtime.Version != "v2" {
		t.Fatalf("old load repopulated current generation with %q", runtime.Version)
	}
	if calls := delegate.loadCount(); calls != 2 {
		t.Fatalf("delegate loads = %d, want separate old/new generation loads", calls)
	}
}

func TestCachedStoreRuntimeInvalidationFansOutAcrossNATSConnections(t *testing.T) {
	natsServer := startRuntimeCacheNATSServer(t)
	firstClient := connectRuntimeCacheNATS(t, natsServer.ClientURL())
	secondClient := connectRuntimeCacheNATS(t, natsServer.ClientURL())

	appID := uuid.New()
	firstDelegate := newRuntimeCacheDelegate(AppRuntime{AppID: appID, Version: "v1"})
	secondDelegate := newRuntimeCacheDelegate(AppRuntime{AppID: appID, Version: "v1"})
	first := NewCachedStore(firstDelegate, firstClient).(*cachedStore)
	second := NewCachedStore(secondDelegate, secondClient).(*cachedStore)
	flushRuntimeCacheNATS(t, firstClient.Conn)
	flushRuntimeCacheNATS(t, secondClient.Conn)

	mustGetRuntime(t, first, appID)
	mustGetRuntime(t, second, appID)
	secondDelegate.setRuntime(AppRuntime{AppID: appID, Version: "v2"})
	first.NotifyAppRuntimeChanged(context.Background(), appID)
	flushRuntimeCacheNATS(t, firstClient.Conn)
	flushRuntimeCacheNATS(t, secondClient.Conn)

	// The peer subscription callback is dispatched asynchronously by the NATS
	// client, so wait for the invalidation to be observed instead of racing the
	// flush round trip against callback delivery.
	deadline := time.Now().Add(time.Second)
	for {
		runtime := mustGetRuntime(t, second, appID)
		if runtime.Version == "v2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer runtime version after NATS invalidation = %q, want v2", runtime.Version)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls := secondDelegate.loadCount(); calls != 2 {
		t.Fatalf("peer delegate loads = %d, want cache fill plus post-event reload", calls)
	}
}

func TestCachedStoreWorkspaceBindingsCachesCopiesAndInvalidatesAfterMutation(t *testing.T) {
	bucketID := uuid.New()
	serviceID := uuid.New()
	versionID := uuid.New()
	literal := "old"
	delegate := newRuntimeConfigurationDelegate()
	delegate.bindings = []WorkspaceConnectionBinding{{
		ServiceID:        serviceID,
		ServiceVersionID: versionID,
		LiteralValue:     &literal,
		OperationIDs:     []string{"getIssue"},
	}}
	cached := NewCachedStore(delegate, nil).(WorkspaceProfileStore)

	first := mustGetBindings(t, cached, bucketID, serviceID, versionID)
	*first[0].LiteralValue = "mutated"
	first[0].OperationIDs[0] = "mutated"
	second := mustGetBindings(t, cached, bucketID, serviceID, versionID)
	if *second[0].LiteralValue != "old" || second[0].OperationIDs[0] != "getIssue" {
		t.Fatalf("cached bindings were mutated through caller copy: %#v", second)
	}
	if calls := delegate.bindingLoadCount(); calls != 1 {
		t.Fatalf("binding delegate loads = %d, want 1", calls)
	}

	updated := "new"
	_, err := cached.UpsertWorkspaceProfileOverride(context.Background(), WorkspaceConnectionProfile{
		ServiceID: serviceID, ServiceVersionID: versionID, AuthType: "api_key",
	}, []WorkspaceConnectionBinding{{
		ServiceID: serviceID, ServiceVersionID: versionID, LiteralValue: &updated,
	}})
	if err != nil {
		t.Fatalf("UpsertWorkspaceProfileOverride: %v", err)
	}
	afterWrite := mustGetBindings(t, cached, bucketID, serviceID, versionID)
	if len(afterWrite) != 1 || afterWrite[0].LiteralValue == nil || *afterWrite[0].LiteralValue != "new" {
		t.Fatalf("bindings after mutation = %#v, want new committed value", afterWrite)
	}
	if calls := delegate.bindingLoadCount(); calls != 2 {
		t.Fatalf("binding delegate loads after invalidation = %d, want 2", calls)
	}
}

func TestCachedStoreWorkspaceBindingResetAndReconcileInvalidate(t *testing.T) {
	bucketID, serviceID, versionID := uuid.New(), uuid.New(), uuid.New()
	literal := "initial"
	delegate := newRuntimeConfigurationDelegate()
	delegate.bindings = []WorkspaceConnectionBinding{{LiteralValue: &literal}}
	cached := NewCachedStore(delegate, nil)
	profiles := cached.(WorkspaceProfileStore)
	batch := cached.(WorkspaceProfileBatchStore)
	mustGetBindings(t, profiles, bucketID, serviceID, versionID)

	if err := profiles.ResetWorkspaceProfile(context.Background(), serviceID, versionID, "api_key"); err != nil {
		t.Fatalf("ResetWorkspaceProfile: %v", err)
	}
	if bindings := mustGetBindings(t, profiles, bucketID, serviceID, versionID); len(bindings) != 0 {
		t.Fatalf("bindings after reset = %#v, want empty fallback", bindings)
	}
	updated := "reconciled"
	replacements := []WorkspaceProfileReplacement{{Bindings: []WorkspaceConnectionBinding{{LiteralValue: &updated}}}}
	if err := batch.ReconcileWorkspaceProfiles(context.Background(), replacements, nil); err != nil {
		t.Fatalf("ReconcileWorkspaceProfiles: %v", err)
	}
	bindings := mustGetBindings(t, profiles, bucketID, serviceID, versionID)
	if len(bindings) != 1 || bindings[0].LiteralValue == nil || *bindings[0].LiteralValue != updated {
		t.Fatalf("bindings after reconcile = %#v", bindings)
	}
	if calls := delegate.bindingLoadCount(); calls != 3 {
		t.Fatalf("binding delegate loads = %d, want fill + reset reload + reconcile reload", calls)
	}
}

func TestCachedStoreExecutionPolicyCachesNil(t *testing.T) {
	serviceID := uuid.New()
	versionID := uuid.New()
	delegate := newRuntimeConfigurationDelegate()
	cached := NewCachedStore(delegate, nil).(WorkspaceExecutionPolicyStore)

	requireNilRuntimePolicy(t, cached, serviceID, versionID)
	requireNilRuntimePolicy(t, cached, serviceID, versionID)
	if calls := delegate.policyLoadCount(); calls != 1 {
		t.Fatalf("nil policy delegate loads = %d, want 1", calls)
	}
}

func TestCachedStoreExecutionPolicyInvalidatesWritesAndCopiesNestedValues(t *testing.T) {
	serviceID := uuid.New()
	versionID := uuid.New()
	delegate := newRuntimeConfigurationDelegate()
	cached := NewCachedStore(delegate, nil).(WorkspaceExecutionPolicyStore)
	requireNilRuntimePolicy(t, cached, serviceID, versionID)

	timeout := 250
	stored, err := cached.UpsertWorkspaceExecutionPolicyOverride(context.Background(), WorkspaceExecutionPolicyOverride{
		ServiceID: serviceID, ServiceVersionID: &versionID, TimeoutMs: &timeout,
		RateLimit: testWorkspaceRateLimit(100), ServerVariables: map[string]string{"region": "eu"},
	})
	if err != nil {
		t.Fatalf("UpsertWorkspaceExecutionPolicyOverride: %v", err)
	}
	if stored == nil {
		t.Fatal("UpsertWorkspaceExecutionPolicyOverride returned nil")
	}
	afterUpsert := mustGetPolicy(t, cached, serviceID, versionID)
	requireRuntimePolicyValues(t, afterUpsert, 250, "eu", 100)
	afterUpsert.ServerVariables["region"] = "mutated"
	afterUpsert.RateLimit.Policies[0].FixedWindow.Limit = 999
	requireRuntimePolicyValues(t, mustGetPolicy(t, cached, serviceID, versionID), 250, "eu", 100)

	if err := cached.ResetWorkspaceExecutionPolicyOverride(context.Background(), serviceID, &versionID); err != nil {
		t.Fatalf("ResetWorkspaceExecutionPolicyOverride: %v", err)
	}
	requireNilRuntimePolicy(t, cached, serviceID, versionID)
	if calls := delegate.policyLoadCount(); calls != 3 {
		t.Fatalf("policy delegate loads = %d, want nil fill + upsert reload + reset reload", calls)
	}
}

func TestCachedStoreHardDeactivationInvalidatesOnlyAfterCommit(t *testing.T) {
	appID := uuid.New()
	actorID := uuid.New()

	t.Run("failed transaction keeps cache", func(t *testing.T) {
		delegate := newRuntimeCacheDelegate(AppRuntime{AppID: appID, Version: "v1"})
		cached := NewCachedStore(delegate, nil).(*cachedStore)
		mustGetRuntime(t, cached, appID)
		delegate.setRuntime(AppRuntime{AppID: appID, Version: "v2"})
		delegate.setDeactivateError(errors.New("transaction rolled back"))

		if err := cached.DeactivateAppVersion(context.Background(), appID, actorID); err == nil {
			t.Fatal("DeactivateAppVersion succeeded despite delegate failure")
		}
		if runtime := mustGetRuntime(t, cached, appID); runtime.Version != "v1" {
			t.Fatalf("failed deactivation invalidated cache to %q, want v1", runtime.Version)
		}
	})

	t.Run("committed tombstone evicts runtime", func(t *testing.T) {
		delegate := newRuntimeCacheDelegate(AppRuntime{AppID: appID, Version: "v1"})
		cached := NewCachedStore(delegate, nil).(*cachedStore)
		mustGetRuntime(t, cached, appID)

		if err := cached.DeactivateAppVersion(context.Background(), appID, actorID); err != nil {
			t.Fatalf("DeactivateAppVersion: %v", err)
		}
		if _, err := cached.GetAppRuntime(context.Background(), appID); !errors.Is(err, ErrAppRuntimeNotFound) {
			t.Fatalf("GetAppRuntime after deactivation error = %v, want %v", err, ErrAppRuntimeNotFound)
		}
		if calls := delegate.loadCount(); calls != 2 {
			t.Fatalf("runtime delegate loads = %d, want cache fill plus post-deactivation lookup", calls)
		}
	})
}

func TestCachedStoreAppStatusAndFamilyBucketMutationsInvalidate(t *testing.T) {
	appID, familyID := uuid.New(), uuid.New()
	firstBucket, secondBucket := uuid.New(), uuid.New()
	delegate := newRuntimeCacheDelegate(AppRuntime{AppID: appID, BucketID: firstBucket, Status: AppStatusActive})
	cached := NewCachedStore(delegate, nil).(*cachedStore)
	mustGetRuntime(t, cached, appID)

	if err := cached.DeprecateApp(context.Background(), appID, "upgrade", nil); err != nil {
		t.Fatalf("DeprecateApp: %v", err)
	}
	requireRuntimeStatus(t, cached, appID, AppStatusDeprecated)
	if err := cached.UndeprecateApp(context.Background(), appID); err != nil {
		t.Fatalf("UndeprecateApp: %v", err)
	}
	requireRuntimeStatus(t, cached, appID, AppStatusActive)
	if err := cached.SetAppFamilyBucket(context.Background(), familyID, secondBucket); err != nil {
		t.Fatalf("SetAppFamilyBucket: %v", err)
	}
	if runtime := mustGetRuntime(t, cached, appID); runtime.BucketID != secondBucket {
		t.Fatalf("runtime bucket = %s, want %s", runtime.BucketID, secondBucket)
	}
	if calls := delegate.loadCount(); calls != 4 {
		t.Fatalf("runtime delegate loads = %d, want one per committed mutation generation", calls)
	}
}

func TestCachedStoreSuccessfulMutationRecordsBoundedInvalidationEvent(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ctx, span := provider.Tracer("runtime-cache-test").Start(context.Background(), "workspace.profile.upsert")

	delegate := newRuntimeConfigurationDelegate()
	cached := NewCachedStore(delegate, nil).(WorkspaceProfileStore)
	serviceID := uuid.New()
	versionID := uuid.New()
	_, err := cached.UpsertWorkspaceProfileOverride(ctx, WorkspaceConnectionProfile{
		ServiceID: serviceID, ServiceVersionID: versionID, AuthType: "api_key",
	}, nil)
	span.End()
	if err != nil {
		t.Fatalf("UpsertWorkspaceProfileOverride: %v", err)
	}

	assertBoundedRuntimeInvalidationEvent(t, recorder.Ended(), serviceID, versionID)
}

type runtimeCacheDelegate struct {
	Store
	mu            sync.Mutex
	runtime       AppRuntime
	calls         int
	started       chan struct{}
	startedOnce   sync.Once
	release       <-chan struct{}
	runtimeErr    error
	deactivateErr error
}

type runtimeGenerationFenceDelegate struct {
	Store
	mu           sync.Mutex
	appID        uuid.UUID
	calls        int
	firstStarted chan struct{}
	firstRelease chan struct{}
}

// newRuntimeGenerationFenceDelegate makes only the first source read block so
// a post-invalidation generation can prove it does not join the old leader.
func newRuntimeGenerationFenceDelegate(appID uuid.UUID) *runtimeGenerationFenceDelegate {
	return &runtimeGenerationFenceDelegate{
		appID: appID, firstStarted: make(chan struct{}), firstRelease: make(chan struct{}),
	}
}

// GetAppRuntime returns v1 to the fenced read and v2 to later reads, modeling a
// write committed while the first database snapshot was already in flight.
func (d *runtimeGenerationFenceDelegate) GetAppRuntime(ctx context.Context, _ uuid.UUID) (*AppRuntime, error) {
	d.mu.Lock()
	d.calls++
	call := d.calls
	d.mu.Unlock()
	version := "v2"
	if call == 1 {
		version = "v1"
		close(d.firstStarted)
		select {
		case <-d.firstRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &AppRuntime{AppID: d.appID, Version: version}, nil
}

// loadCount synchronizes the generation-fence query count with both loader
// goroutines so the test remains race-detector clean.
func (d *runtimeGenerationFenceDelegate) loadCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// newRuntimeCacheDelegate clones its input so caller mutations cannot make a
// defensive-copy regression pass without exercising the cache boundary.
func newRuntimeCacheDelegate(runtime AppRuntime) *runtimeCacheDelegate {
	return &runtimeCacheDelegate{
		runtime: cloneAppRuntime(runtime),
		started: make(chan struct{}),
	}
}

// GetAppRuntime captures one consistent database-like snapshot before an
// optional test fence holds the load open for singleflight contenders.
func (d *runtimeCacheDelegate) GetAppRuntime(ctx context.Context, _ uuid.UUID) (*AppRuntime, error) {
	d.mu.Lock()
	d.calls++
	runtime := cloneAppRuntime(d.runtime)
	release := d.release
	runtimeErr := d.runtimeErr
	d.mu.Unlock()
	d.startedOnce.Do(func() { close(d.started) })

	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &runtime, runtimeErr
}

// setRuntime simulates a committed write that could miss best-effort NATS,
// allowing the absolute-TTL test to prove bounded stale state independently.
func (d *runtimeCacheDelegate) setRuntime(runtime AppRuntime) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtime = cloneAppRuntime(runtime)
}

// setDeactivateError lets the transaction-failure case prove invalidation is
// ordered after persistence without exposing implementation internals.
func (d *runtimeCacheDelegate) setDeactivateError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deactivateErr = err
}

// DeactivateAppVersion models the hard-delete boundary by making every future
// runtime lookup fail only after the mutation commits successfully.
func (d *runtimeCacheDelegate) DeactivateAppVersion(context.Context, uuid.UUID, uuid.UUID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deactivateErr != nil {
		return d.deactivateErr
	}
	d.runtimeErr = ErrAppRuntimeNotFound
	return nil
}

// DeprecateApp updates the fake's projected runtime status at the same commit
// boundary exercised by the cache wrapper.
func (d *runtimeCacheDelegate) DeprecateApp(context.Context, uuid.UUID, string, *time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtime.Status = AppStatusDeprecated
	return nil
}

// UndeprecateApp restores active status so both lifecycle transition wrappers
// prove they invalidate the cached projection.
func (d *runtimeCacheDelegate) UndeprecateApp(context.Context, uuid.UUID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtime.Status = AppStatusActive
	return nil
}

// SetAppFamilyBucket changes the joined family value without looking up apps,
// matching the production wrapper's broad invalidation decision.
func (d *runtimeCacheDelegate) SetAppFamilyBucket(_ context.Context, _, bucketID uuid.UUID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtime.BucketID = bucketID
	return nil
}

// loadCount reads the fake's query count under the same lock used by loaders
// so concurrent coalescing checks remain race-detector clean.
func (d *runtimeCacheDelegate) loadCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// mustGetRuntime keeps failure handling out of timing loops so those loops
// express only the stale-data condition they are intended to exercise.
func mustGetRuntime(t *testing.T, cached Store, appID uuid.UUID) *AppRuntime {
	t.Helper()
	runtime, err := cached.GetAppRuntime(context.Background(), appID)
	if err != nil {
		t.Fatalf("GetAppRuntime: %v", err)
	}
	return runtime
}

// requireRuntimeStatus isolates lifecycle assertions from mutation error
// handling so the covering test remains below the complexity ceiling.
func requireRuntimeStatus(t *testing.T, cached Store, appID uuid.UUID, want AppStatus) {
	t.Helper()
	if runtime := mustGetRuntime(t, cached, appID); runtime.Status != want {
		t.Fatalf("runtime status = %q, want %q", runtime.Status, want)
	}
}

type runtimeConfigurationDelegate struct {
	Store
	mu           sync.Mutex
	bindings     []WorkspaceConnectionBinding
	bindingCalls int
	policy       *WorkspaceExecutionPolicyOverride
	policyCalls  int
}

// newRuntimeConfigurationDelegate starts with no policy so tests cover the
// cacheable nil fallback, which is the common no-workspace-override path.
func newRuntimeConfigurationDelegate() *runtimeConfigurationDelegate {
	return &runtimeConfigurationDelegate{}
}

// ListWorkspaceBindingsForExecution returns the already SQL-filtered shape;
// the fake intentionally does no Go-side filtering that could mask N+1 work.
func (d *runtimeConfigurationDelegate) ListWorkspaceBindingsForExecution(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string) ([]WorkspaceConnectionBinding, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bindingCalls++
	return cloneWorkspaceBindings(d.bindings), nil
}

// UpsertWorkspaceProfileOverride replaces the complete effective result to
// mirror the production transaction and exercise post-commit invalidation.
func (d *runtimeConfigurationDelegate) UpsertWorkspaceProfileOverride(_ context.Context, profile WorkspaceConnectionProfile, bindings []WorkspaceConnectionBinding) (*WorkspaceConnectionProfile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bindings = cloneWorkspaceBindings(bindings)
	return &profile, nil
}

// ResetWorkspaceProfile models the baseline fallback as an empty effective
// binding set; this method exists to satisfy the narrow profile capability.
func (d *runtimeConfigurationDelegate) ResetWorkspaceProfile(context.Context, uuid.UUID, uuid.UUID, string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bindings = nil
	return nil
}

// ReconcileWorkspaceProfiles replaces the complete fake result as one batch,
// preserving the single transaction boundary that the wrapper invalidates.
func (d *runtimeConfigurationDelegate) ReconcileWorkspaceProfiles(_ context.Context, replacements []WorkspaceProfileReplacement, _ []WorkspaceProfileRef) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bindings = nil
	if len(replacements) > 0 {
		d.bindings = cloneWorkspaceBindings(replacements[0].Bindings)
	}
	return nil
}

// GetEffectiveWorkspaceProfile is outside the cached execution surface and is
// deliberately inert in this focused fake.
func (d *runtimeConfigurationDelegate) GetEffectiveWorkspaceProfile(context.Context, uuid.UUID, uuid.UUID, string) (*WorkspaceConnectionProfile, error) {
	return nil, nil
}

// GetEffectiveWorkspaceProfiles preserves the batch capability without adding
// unrelated state to the runtime-cache tests.
func (d *runtimeConfigurationDelegate) GetEffectiveWorkspaceProfiles(context.Context, []WorkspaceProfileRef) ([]WorkspaceConnectionProfile, error) {
	return nil, nil
}

// ListWorkspaceProfileBindings is an administrative read and stays inert so
// execution-cache call counts measure only the hot-path delegate method.
func (d *runtimeConfigurationDelegate) ListWorkspaceProfileBindings(context.Context, uuid.UUID, uuid.UUID, string) ([]WorkspaceConnectionBinding, error) {
	return nil, nil
}

// MarkWorkspaceProfilePublished does not affect runtime bindings, matching the
// production wrapper's decision not to invalidate for bookkeeping alone.
func (d *runtimeConfigurationDelegate) MarkWorkspaceProfilePublished(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}

// GetEffectiveWorkspaceExecutionPolicyOverride returns a cloned immutable DTO
// so caller mutation tests exercise the cache rather than the fake repository.
func (d *runtimeConfigurationDelegate) GetEffectiveWorkspaceExecutionPolicyOverride(context.Context, uuid.UUID, uuid.UUID) (*WorkspaceExecutionPolicyOverride, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.policyCalls++
	return cloneRuntimePolicyForTest(d.policy), nil
}

// UpsertWorkspaceExecutionPolicyOverride commits the new effective value in
// one step before the wrapper publishes invalidation.
func (d *runtimeConfigurationDelegate) UpsertWorkspaceExecutionPolicyOverride(_ context.Context, override WorkspaceExecutionPolicyOverride) (*WorkspaceExecutionPolicyOverride, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.policy = cloneRuntimePolicyForTest(&override)
	return cloneRuntimePolicyForTest(d.policy), nil
}

// ResetWorkspaceExecutionPolicyOverride removes the row so the next cached
// lookup must observe the nil service-contract fallback.
func (d *runtimeConfigurationDelegate) ResetWorkspaceExecutionPolicyOverride(context.Context, uuid.UUID, *uuid.UUID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.policy = nil
	return nil
}

// bindingLoadCount is synchronized because invalidation tests may be extended
// to exercise fanout callbacks running on NATS goroutines.
func (d *runtimeConfigurationDelegate) bindingLoadCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bindingCalls
}

// policyLoadCount keeps nil/non-nil cache assertions race-detector safe.
func (d *runtimeConfigurationDelegate) policyLoadCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.policyCalls
}

// cloneRuntimePolicyForTest keeps the repository fake independent from the
// cache's wire encoding while still preventing shared mutable test state.
func cloneRuntimePolicyForTest(policy *WorkspaceExecutionPolicyOverride) *WorkspaceExecutionPolicyOverride {
	if policy == nil {
		return nil
	}
	cloned := *policy
	cloned.ServiceVersionID = cloneRuntimeUUIDForTest(policy.ServiceVersionID)
	cloned.TimeoutMs = cloneRuntimeIntForTest(policy.TimeoutMs)
	cloned.ServerVariables = cloneRuntimeMapForTest(policy.ServerVariables)
	return &cloned
}

// cloneRuntimeUUIDForTest prevents a version selector in fake persistence from
// aliasing the request DTO supplied by a test caller.
func cloneRuntimeUUIDForTest(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// cloneRuntimeIntForTest gives timeout mutations the same isolation expected
// from decoding a fresh PostgreSQL row.
func cloneRuntimeIntForTest(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// cloneRuntimeMapForTest copies server variables because maps otherwise make
// a cache defensive-copy failure indistinguishable from a fake-store failure.
func cloneRuntimeMapForTest(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

// mustGetBindings centralizes the exact cache key inputs so the test cannot
// accidentally miss the cache by varying an unrelated selector.
func mustGetBindings(t *testing.T, cached WorkspaceProfileStore, bucketID, serviceID, versionID uuid.UUID) []WorkspaceConnectionBinding {
	t.Helper()
	bindings, err := cached.ListWorkspaceBindingsForExecution(context.Background(), bucketID, serviceID, versionID, "api_key", "getIssue")
	if err != nil {
		t.Fatalf("ListWorkspaceBindingsForExecution: %v", err)
	}
	return bindings
}

// mustGetPolicy keeps cache-state assertions separate from repetitive error
// handling and preserves the exact service/version cache key.
func mustGetPolicy(t *testing.T, cached WorkspaceExecutionPolicyStore, serviceID, versionID uuid.UUID) *WorkspaceExecutionPolicyOverride {
	t.Helper()
	policy, err := cached.GetEffectiveWorkspaceExecutionPolicyOverride(context.Background(), serviceID, versionID)
	if err != nil {
		t.Fatalf("GetEffectiveWorkspaceExecutionPolicyOverride: %v", err)
	}
	return policy
}

// requireNilRuntimePolicy keeps nil-fallback assertions reusable without
// inflating the mutation test's decision complexity.
func requireNilRuntimePolicy(t *testing.T, cached WorkspaceExecutionPolicyStore, serviceID, versionID uuid.UUID) {
	t.Helper()
	if policy := mustGetPolicy(t, cached, serviceID, versionID); policy != nil {
		t.Fatalf("policy = %#v, want nil", policy)
	}
}

// requireRuntimePolicyValues checks independent wrapper and nested values so
// a cache-copy regression produces a precise failure without compound logic.
func requireRuntimePolicyValues(t *testing.T, policy *WorkspaceExecutionPolicyOverride, timeout int, region string, limit int64) {
	t.Helper()
	if policy == nil {
		t.Fatal("policy is nil")
	}
	if policy.TimeoutMs == nil || *policy.TimeoutMs != timeout {
		t.Fatalf("policy timeout = %#v, want %d", policy.TimeoutMs, timeout)
	}
	if policy.ServerVariables["region"] != region {
		t.Fatalf("policy region = %q, want %q", policy.ServerVariables["region"], region)
	}
	if policy.RateLimit == nil || len(policy.RateLimit.Policies) == 0 {
		t.Fatalf("policy rate limit = %#v, want one policy", policy.RateLimit)
	}
	fixedWindow := policy.RateLimit.Policies[0].FixedWindow
	if fixedWindow == nil || fixedWindow.Limit != limit {
		t.Fatalf("policy fixed window = %#v, want limit %d", fixedWindow, limit)
	}
}

// assertBoundedRuntimeInvalidationEvent proves mutation telemetry contains only
// stable cache classifications and cannot expose service/version identifiers.
func assertBoundedRuntimeInvalidationEvent(t *testing.T, spans []sdktrace.ReadOnlySpan, prohibited ...uuid.UUID) {
	t.Helper()
	for _, span := range spans {
		for _, event := range span.Events() {
			if event.Name != "engine.runtime_cache.invalidated" {
				continue
			}
			if len(event.Attributes) != 2 {
				t.Fatalf("invalidation event attributes = %#v, want two bounded fields", event.Attributes)
			}
			fields := map[string]string{}
			serialized := event.Name
			for _, attr := range event.Attributes {
				fields[string(attr.Key)] = attr.Value.AsString()
				serialized += string(attr.Key) + attr.Value.AsString()
			}
			if fields["cache.kind"] != "runtime_configuration" || fields["cache.propagation"] != "unavailable" {
				t.Fatalf("invalidation event fields = %#v", fields)
			}
			for _, id := range prohibited {
				if strings.Contains(serialized, id.String()) {
					t.Fatalf("invalidation event exposed identifier %s", id)
				}
			}
			return
		}
	}
	t.Fatal("runtime invalidation event was not recorded")
}

// startRuntimeCacheNATSServer uses an ephemeral port so the fanout test can run
// beside developer and CI NATS processes without sharing external state.
func startRuntimeCacheNATSServer(t *testing.T) *server.Server {
	t.Helper()
	natsServer, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(),
		NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatalf("create NATS server: %v", err)
	}
	go natsServer.Start()
	if !natsServer.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server did not become ready")
	}
	t.Cleanup(natsServer.Shutdown)
	return natsServer
}

// connectRuntimeCacheNATS opens distinct connections because sharing one
// connection would not prove invalidation reaches a peer Engine process.
func connectRuntimeCacheNATS(t *testing.T, url string) *messaging.NATSClient {
	t.Helper()
	connection, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(connection.Close)
	return &messaging.NATSClient{Conn: connection}
}

// flushRuntimeCacheNATS establishes subscription and delivery ordering without
// adding arbitrary sleeps to the cross-instance invalidation assertion.
func flushRuntimeCacheNATS(t *testing.T, connection *nats.Conn) {
	t.Helper()
	if err := connection.FlushTimeout(time.Second); err != nil {
		t.Fatalf("flush NATS: %v", err)
	}
}
