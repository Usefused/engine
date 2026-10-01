package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/shared/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const capabilityWorkerIdleTime = 30 * time.Second
const capabilityWorkerCapacity = 4
const capabilityWorkerQueueLimit = 32

// ErrCapabilityWorkerOverloaded lets callers distinguish a full per-family queue from app failure.
var ErrCapabilityWorkerOverloaded = errors.New("unified app worker queue is full")

// ErrCapabilityWorkerConcurrencyDisabled marks a plan that admits no Unified App invocations.
var ErrCapabilityWorkerConcurrencyDisabled = errors.New("unified app concurrency is disabled by plan")

// CapabilityWarmTarget identifies the exact immutable version that should remain resident.
type CapabilityWarmTarget struct {
	FamilyID string
	AppID    string
	Bundle   []byte
}

// CapabilityWorkerManager owns at most one confined process for each app family.
type CapabilityWorkerManager struct {
	mu           sync.Mutex
	cond         *sync.Cond
	entries      map[string]*capabilityWorkerEntry
	ctx          context.Context
	cancel       context.CancelFunc
	closed       bool
	admission    *capabilityAdmission
	admissionErr error
}

type capabilityWorkerEntry struct {
	appID    string
	digest   [32]byte
	worker   *persistentCapabilityWorker
	loading  bool
	refs     int
	draining bool
	keepWarm bool
	idle     *time.Timer
}

// NewCapabilityWorkerManager creates the Engine-owned family worker registry without starting authored code.
func NewCapabilityWorkerManager() *CapabilityWorkerManager {
	gate, err := configuredCapabilityAdmission()
	return newCapabilityWorkerManager(gate, err)
}

// NewCapabilityWorkerManagerWithConfig uses the policy already resolved from engine.yaml and its overrides.
func NewCapabilityWorkerManagerWithConfig(policy config.UnifiedAppAdmissionConfig) *CapabilityWorkerManager {
	gate, err := capabilityAdmissionFromConfig(policy)
	return newCapabilityWorkerManager(gate, err)
}

// newCapabilityWorkerManager centralizes lifecycle initialization independently of configuration provenance.
func newCapabilityWorkerManager(gate *capabilityAdmission, err error) *CapabilityWorkerManager {
	ctx, cancel := context.WithCancel(context.Background())
	manager := &CapabilityWorkerManager{entries: make(map[string]*capabilityWorkerEntry), ctx: ctx, cancel: cancel, admission: gate, admissionErr: err}
	manager.cond = sync.NewCond(&manager.mu)
	return manager
}

// Run executes one request in the loaded family worker with request-local authority and a fresh JavaScript realm.
func (manager *CapabilityWorkerManager) Run(ctx context.Context, familyID, appID string, bundle []byte, input json.RawMessage, host CapabilityScriptHost, control CapabilityDeterminism, keepWarm bool, planConcurrency *int) (json.RawMessage, error) {
	// Invalid authority or replay inputs never start or reuse a resident sandbox.
	if familyID == "" || appID == "" || host == nil || len(input) > maxCapabilityInputBytes || !json.Valid(input) || !control.Valid() {
		return nil, errors.New("capability invocation is invalid")
	}
	limit := effectiveCapabilityWorkerConcurrency(planConcurrency)
	// A zero plan limit must not start authored code or consume a worker slot.
	if limit == 0 {
		return nil, ErrCapabilityWorkerConcurrencyDisabled
	}
	// Deployment admission precedes cold worker creation, bounding startup and execution together.
	if manager.admissionErr != nil {
		return nil, manager.admissionErr
	}
	release, err := manager.admission.acquire(ctx, manager.ctx, familyID, limit)
	// Queue rejection must not create or acquire an app worker.
	if err != nil {
		return nil, err
	}
	entry, err := manager.acquire(ctx, familyID, appID, bundle, keepWarm)
	// A failed load never transfers lease ownership to a child.
	if err != nil {
		release()
		return nil, err
	}
	defer manager.release(familyID, entry)
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.unified_app.worker.run", trace.WithAttributes(
		attribute.String("app.family_id", familyID), attribute.String("app.id", appID),
		attribute.Int("worker.pid", entry.worker.command.Process.Pid), attribute.Int("worker.concurrency_limit", limit),
	))
	defer span.End()
	return entry.worker.run(ctx, input, host, control, limit, release)
}

// effectiveCapabilityWorkerConcurrency applies the plan while preserving the process safety ceiling.
func effectiveCapabilityWorkerConcurrency(planConcurrency *int) int {
	// Older Registry bundles and unlimited legacy values use the process's tested capacity.
	if planConcurrency == nil || *planConcurrency < 0 || *planConcurrency > capabilityWorkerCapacity {
		return capabilityWorkerCapacity
	}
	return *planConcurrency
}

// Warm validates and loads one exact app version without creating an execution record.
func (manager *CapabilityWorkerManager) Warm(ctx context.Context, familyID, appID string, bundle []byte) error {
	entry, err := manager.acquire(ctx, familyID, appID, bundle, true)
	if err != nil {
		return err
	}
	manager.release(familyID, entry)
	return nil
}

// Evict drains current callers before replacing or removing one family's worker.
func (manager *CapabilityWorkerManager) Evict(familyID string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	entry := manager.entries[familyID]
	// A missing family has no sandbox to retire.
	if entry == nil {
		return
	}
	entry.draining = true
	for entry.refs > 0 {
		manager.cond.Wait()
	}
	// A concurrent promotion may have already replaced the observed entry.
	if manager.entries[familyID] != entry {
		return
	}
	manager.removeLocked(familyID, entry)
}

// ReconcileWarmTargets keeps selected versions resident and lets unlisted workers retire when idle.
func (manager *CapabilityWorkerManager) ReconcileWarmTargets(ctx context.Context, targets []CapabilityWarmTarget) error {
	var failures []error
	wanted := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		wanted[target.FamilyID] = struct{}{}
	}
	manager.mu.Lock()
	for familyID, entry := range manager.entries {
		// A plan downgrade must not interrupt an active invocation.
		if _, keep := wanted[familyID]; !keep {
			entry.keepWarm = false
			manager.scheduleIdleLocked(familyID, entry)
		}
	}
	manager.mu.Unlock()
	for _, target := range targets {
		// One broken app must not prevent healthy families from becoming ready.
		if err := manager.Warm(ctx, target.FamilyID, target.AppID, target.Bundle); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// SetAlwaysOn demotes resident workers when the plan loses its warm sandbox entitlement.
func (manager *CapabilityWorkerManager) SetAlwaysOn(enabled bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	// Enabling is driven by exact warm targets, so unrelated Dev traffic is not pinned.
	if enabled {
		return
	}
	for familyID, entry := range manager.entries {
		entry.keepWarm = false
		manager.scheduleIdleLocked(familyID, entry)
	}
}

// Close terminates all resident children when the Engine shuts down.
func (manager *CapabilityWorkerManager) Close() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	// Repeated shutdown calls must not race sandbox cleanup or close the context twice.
	if manager.closed {
		return
	}
	manager.closed = true
	manager.cancel()
	for familyID, entry := range manager.entries {
		manager.removeLocked(familyID, entry)
	}
	manager.cond.Broadcast()
}

// acquire binds a caller to the exact loaded version and serializes version replacement.
func (manager *CapabilityWorkerManager) acquire(ctx context.Context, familyID, appID string, bundle []byte, keepWarm bool) (*capabilityWorkerEntry, error) {
	// A family can never load a zero-length or unbounded authored bundle.
	if !validCapabilityWorkerTarget(familyID, appID, bundle) {
		return nil, errors.New("capability invocation is invalid")
	}
	digest := sha256.Sum256(bundle)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	stopWake := context.AfterFunc(ctx, func() {
		manager.mu.Lock()
		manager.cond.Broadcast()
		manager.mu.Unlock()
	})
	defer stopWake()
	for {
		// A waiting promotion must respect caller cancellation and Engine shutdown.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if manager.closed {
			return nil, ErrCapabilityWorkerUnavailable
		}
		entry := manager.entries[familyID]
		if entry == nil {
			return manager.loadEntryLocked(ctx, familyID, appID, bundle, digest, keepWarm)
		}
		// A version identifier may never point to different executable bytes.
		if immutableCapabilityBundleChanged(entry, appID, digest) {
			return nil, errors.New("unified app bundle changed for an immutable version")
		}
		// In-flight startup owns this reservation until a validated child or failure is published.
		if entry.draining || entry.loading {
			manager.cond.Wait()
			continue
		}
		// A dead sandbox is replaced before any new invocation acquires it.
		if capabilityWorkerNeedsReplacement(entry, appID) {
			manager.retireEntryLocked(familyID, entry)
			continue
		}
		manager.reuseEntryLocked(entry, keepWarm)
		return entry, nil
	}
}

// validCapabilityWorkerTarget keeps family identity and source size bounded before acquisition.
func validCapabilityWorkerTarget(familyID, appID string, bundle []byte) bool {
	return familyID != "" && appID != "" && len(bundle) > 0 && len(bundle) <= maxCapabilityBundleBytes
}

// immutableCapabilityBundleChanged prevents a version ID from silently changing executable bytes.
func immutableCapabilityBundleChanged(entry *capabilityWorkerEntry, appID string, digest [32]byte) bool {
	return entry.appID == appID && entry.digest != digest
}

// capabilityWorkerNeedsReplacement reaps a dead sandbox or a superseded app version.
func capabilityWorkerNeedsReplacement(entry *capabilityWorkerEntry, appID string) bool {
	return entry.appID != appID || entry.worker.isClosed()
}

// reuseEntryLocked pins one existing sandbox while new traffic is admitted.
func (manager *CapabilityWorkerManager) reuseEntryLocked(entry *capabilityWorkerEntry, keepWarm bool) {
	entry.refs++
	entry.keepWarm = keepWarm
	// New traffic cancels a pending Dev idle retirement.
	if entry.idle != nil {
		entry.idle.Stop()
		entry.idle = nil
	}
}

// loadEntryLocked reserves family capacity atomically, then starts the child outside the global registry lock.
func (manager *CapabilityWorkerManager) loadEntryLocked(ctx context.Context, familyID, appID string, bundle []byte, digest [32]byte, keepWarm bool) (*capabilityWorkerEntry, error) {
	limit := entitlement.LiveEntitlement.Load().MaxUnifiedAppFamilies
	// Registry owns account policy; explicit zero blocks and negative/missing limits remain unlimited.
	if limit != nil && *limit >= 0 && len(manager.entries) >= *limit {
		return nil, ErrCapabilityWorkerOverloaded
	}
	entry := &capabilityWorkerEntry{appID: appID, digest: digest, refs: 1, keepWarm: keepWarm, loading: true}
	manager.entries[familyID] = entry
	manager.mu.Unlock()
	start := time.Now()
	worker, err := startPersistentCapabilityWorker(ctx, manager.ctx, bundle)
	manager.mu.Lock()
	// Shutdown or a failed process start removes the reservation before waking other family callers.
	if err != nil || manager.closed {
		entry.refs, entry.loading = 0, false
		delete(manager.entries, familyID)
		manager.cond.Broadcast()
		if worker != nil {
			worker.stop()
		}
		if err != nil {
			return nil, err
		}
		return nil, ErrCapabilityWorkerUnavailable
	}
	entry.worker, entry.loading = worker, false
	manager.cond.Broadcast()
	slog.InfoContext(ctx, "Unified App worker loaded", "app_family_id", familyID, "app_id", appID,
		"worker_pid", worker.command.Process.Pid, "startup_ms", time.Since(start).Milliseconds())
	return entry, nil
}

// retireEntryLocked waits for current callers before removing an old or dead version.
func (manager *CapabilityWorkerManager) retireEntryLocked(familyID string, entry *capabilityWorkerEntry) {
	entry.draining = true
	for entry.refs > 0 {
		manager.cond.Wait()
	}
	// Another eviction may have replaced the observed entry while this caller waited.
	if manager.entries[familyID] != entry {
		return
	}
	manager.removeLocked(familyID, entry)
}

// release permits a waiting version promotion and schedules Dev idle retirement.
func (manager *CapabilityWorkerManager) release(familyID string, entry *capabilityWorkerEntry) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	entry.refs--
	manager.cond.Broadcast()
	// A draining or already removed version is owned by the promotion path.
	if entry.refs == 0 && !entry.draining && manager.entries[familyID] == entry {
		manager.scheduleIdleLocked(familyID, entry)
	}
}

// scheduleIdleLocked starts the small Dev worker retirement window after activity stops.
func (manager *CapabilityWorkerManager) scheduleIdleLocked(familyID string, entry *capabilityWorkerEntry) {
	// Warm versions and active callers remain resident.
	if entry.keepWarm || entry.refs > 0 || entry.draining || manager.closed {
		return
	}
	// Periodic entitlement reconciliation must not keep extending a Dev worker's idle lifetime.
	if entry.idle != nil {
		return
	}
	entry.idle = time.AfterFunc(capabilityWorkerIdleTime, func() { manager.retireIdle(familyID, entry) })
}

// retireIdle removes only the still-current, unused worker for a Dev app family.
func (manager *CapabilityWorkerManager) retireIdle(familyID string, entry *capabilityWorkerEntry) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	// A newer version or newly active caller owns this family now.
	if manager.entries[familyID] != entry || entry.keepWarm || entry.refs > 0 || entry.draining {
		return
	}
	// A canceled caller may have returned before its interpreter acknowledged cancellation.
	if entry.worker.active() > 0 {
		manager.scheduleIdleLocked(familyID, entry)
		return
	}
	manager.removeLocked(familyID, entry)
}

// removeLocked stops one sandbox before allowing a replacement sandbox for its family.
func (manager *CapabilityWorkerManager) removeLocked(familyID string, entry *capabilityWorkerEntry) {
	if entry.idle != nil {
		entry.idle.Stop()
	}
	// A reserved family may still be starting its child when Engine shutdown begins.
	if entry.worker != nil {
		entry.worker.stop()
	}
	delete(manager.entries, familyID)
	manager.cond.Broadcast()
}
