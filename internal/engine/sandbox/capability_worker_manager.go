package sandbox

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const capabilityWorkerIdleTime = 30 * time.Second
const capabilityWorkerCapacity = 4
const capabilityWorkerQueueLimit = 32

// ErrCapabilityWorkerOverloaded lets callers distinguish a full per-family queue from app failure.
var ErrCapabilityWorkerOverloaded = errors.New("execution app worker queue is full")

// ErrCapabilityWorkerConcurrencyDisabled marks a plan that admits no Execution App invocations.
var ErrCapabilityWorkerConcurrencyDisabled = errors.New("execution app concurrency is disabled by plan")

// CapabilityWarmTarget identifies the exact immutable version that should remain resident.
type CapabilityWarmTarget struct {
	FamilyID string
	AppID    string
	Bundle   []byte
}

// CapabilityWorkerManager owns at most one confined worker for each app family.
type CapabilityWorkerManager struct {
	mu      sync.Mutex
	cond    *sync.Cond
	entries map[string]*capabilityWorkerEntry
	ctx     context.Context
	cancel  context.CancelFunc
	closed  bool
}

type capabilityWorkerEntry struct {
	appID    string
	digest   [32]byte
	worker   *persistentCapabilityWorker
	refs     int
	draining bool
	keepWarm bool
	idle     *time.Timer
}

// NewCapabilityWorkerManager creates the Engine-owned process pool without starting authored code.
func NewCapabilityWorkerManager() *CapabilityWorkerManager {
	ctx, cancel := context.WithCancel(context.Background())
	manager := &CapabilityWorkerManager{entries: make(map[string]*capabilityWorkerEntry), ctx: ctx, cancel: cancel}
	manager.cond = sync.NewCond(&manager.mu)
	return manager
}

// Run executes one request in the loaded family worker with its own host and Goja VM.
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
	entry, err := manager.acquire(ctx, familyID, appID, bundle, keepWarm)
	if err != nil {
		return nil, err
	}
	defer manager.release(familyID, entry)
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_app.worker.run", trace.WithAttributes(
		attribute.String("app.family_id", familyID), attribute.String("app.id", appID),
		attribute.Int("worker.pid", entry.worker.command.Process.Pid), attribute.Int("worker.concurrency_limit", limit),
	))
	defer span.End()
	return entry.worker.run(ctx, input, host, control, limit)
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
	// A missing family has no process to terminate.
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
	// Repeated shutdown calls must not race process cleanup or close the context twice.
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
			return nil, errors.New("execution app bundle changed for an immutable version")
		}
		if entry.draining {
			manager.cond.Wait()
			continue
		}
		// A dead child is replaced before any new invocation acquires it.
		if capabilityWorkerNeedsReplacement(entry, appID) {
			manager.retireEntryLocked(familyID, entry)
			continue
		}
		manager.reuseEntryLocked(entry, keepWarm)
		return entry, nil
	}
}

// validCapabilityWorkerTarget keeps process identity and source size bounded before acquisition.
func validCapabilityWorkerTarget(familyID, appID string, bundle []byte) bool {
	return familyID != "" && appID != "" && len(bundle) > 0 && len(bundle) <= maxCapabilityBundleBytes
}

// immutableCapabilityBundleChanged prevents a version ID from silently changing executable bytes.
func immutableCapabilityBundleChanged(entry *capabilityWorkerEntry, appID string, digest [32]byte) bool {
	return entry.appID == appID && entry.digest != digest
}

// capabilityWorkerNeedsReplacement reaps a dead child or a superseded app version.
func capabilityWorkerNeedsReplacement(entry *capabilityWorkerEntry, appID string) bool {
	return entry.appID != appID || entry.worker.isClosed()
}

// reuseEntryLocked pins one existing process while new traffic is admitted.
func (manager *CapabilityWorkerManager) reuseEntryLocked(entry *capabilityWorkerEntry, keepWarm bool) {
	entry.refs++
	entry.keepWarm = keepWarm
	// New traffic cancels a pending Dev idle retirement.
	if entry.idle != nil {
		entry.idle.Stop()
		entry.idle = nil
	}
}

// loadEntryLocked starts one exact version and records bounded startup telemetry.
func (manager *CapabilityWorkerManager) loadEntryLocked(ctx context.Context, familyID, appID string, bundle []byte, digest [32]byte, keepWarm bool) (*capabilityWorkerEntry, error) {
	start := time.Now()
	startCtx, span := otel.Tracer("engine").Start(ctx, "engine.execution_app.worker.start", trace.WithAttributes(
		attribute.String("app.family_id", familyID), attribute.String("app.id", appID),
	))
	worker, err := startPersistentCapabilityWorker(startCtx, manager.ctx, bundle)
	elapsed := time.Since(start)
	span.SetAttributes(attribute.Int64("worker.startup_ms", elapsed.Milliseconds()))
	// Startup failures are observable without logging bundles, inputs, or secrets.
	if err != nil {
		span.RecordError(err)
	}
	span.End()
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "Execution App worker loaded", "app_family_id", familyID, "app_id", appID,
		"worker_pid", worker.command.Process.Pid, "startup_ms", elapsed.Milliseconds())
	entry := &capabilityWorkerEntry{appID: appID, digest: digest, worker: worker, refs: 1, keepWarm: keepWarm}
	manager.entries[familyID] = entry
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

// removeLocked stops one child before allowing a replacement child for its family.
func (manager *CapabilityWorkerManager) removeLocked(familyID string, entry *capabilityWorkerEntry) {
	if entry.idle != nil {
		entry.idle.Stop()
	}
	entry.worker.stop()
	delete(manager.entries, familyID)
	manager.cond.Broadcast()
}

type persistentCapabilityWorker struct {
	command  *exec.Cmd
	stdin    io.WriteCloser
	writer   *capabilityFrameWriter
	cleanup  func()
	queue    chan struct{}
	slots    chan struct{}
	slotCond *sync.Cond
	closed   chan struct{}
	once     sync.Once
	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]*capabilityInvocation
}

type capabilityInvocation struct {
	ctx      context.Context
	host     CapabilityScriptHost
	result   chan capabilityInvocationResult
	ids      map[uint64]struct{}
	ordinals map[int]struct{}
	parallel chan struct{}
}

type capabilityInvocationResult struct {
	output json.RawMessage
	err    error
}

// startPersistentCapabilityWorker launches a confined process and validates its one loaded bundle.
func startPersistentCapabilityWorker(ctx, lifetime context.Context, bundle []byte) (*persistentCapabilityWorker, error) {
	command, cleanup, err := capabilityWorkerCommand(lifetime)
	if err != nil {
		return nil, ErrCapabilityWorkerUnavailable
	}
	stdin, err := command.StdinPipe()
	// Both pipes must be established before untrusted code can start.
	if err != nil {
		cleanup()
		return nil, ErrCapabilityWorkerUnavailable
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cleanup()
		return nil, ErrCapabilityWorkerUnavailable
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		cleanup()
		return nil, ErrCapabilityWorkerUnavailable
	}
	worker := &persistentCapabilityWorker{
		command: command, stdin: stdin, writer: &capabilityFrameWriter{encoder: json.NewEncoder(stdin)}, cleanup: cleanup,
		queue: make(chan struct{}, capabilityWorkerQueueLimit), slots: make(chan struct{}, capabilityWorkerCapacity),
		closed: make(chan struct{}), pending: make(map[uint64]*capabilityInvocation),
	}
	worker.slotCond = sync.NewCond(&worker.mu)
	ready := make(chan error, 1)
	go worker.readLoop(capabilityFrameScanner(stdout), ready)
	if err := worker.writer.write(capabilityProcessFrame{Kind: "load", Bundle: bundle}); err != nil {
		worker.stop()
		return nil, ErrCapabilityWorkerUnavailable
	}
	// Load includes a five-second declaration inspection inside the confined process.
	select {
	case err := <-ready:
		if err != nil {
			worker.stop()
			return nil, err
		}
	case <-ctx.Done():
		worker.stop()
		return nil, ctx.Err()
	case <-time.After(7 * time.Second):
		worker.stop()
		return nil, ErrCapabilityWorkerUnavailable
	}
	return worker, nil
}

// run reserves one interpreter slot and correlates all frames by request ID.
func (worker *persistentCapabilityWorker) run(ctx context.Context, input json.RawMessage, host CapabilityScriptHost, control CapabilityDeterminism, limit int) (json.RawMessage, error) {
	start := time.Now()
	if err := worker.admit(ctx, limit); err != nil {
		return nil, err
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int64("worker.queue_wait_ms", time.Since(start).Milliseconds()))
	defer func() { <-worker.queue }()
	invocation := &capabilityInvocation{
		ctx: ctx, host: host, result: make(chan capabilityInvocationResult, 1),
		ids: make(map[uint64]struct{}, maxCapabilityHostCalls), ordinals: make(map[int]struct{}, maxCapabilityHostCalls),
		parallel: make(chan struct{}, maxCapabilityParallel),
	}
	worker.mu.Lock()
	// Failure after a slot is acquired must release it before returning.
	if worker.isClosedLocked() {
		worker.releaseSlotLocked()
		worker.mu.Unlock()
		return nil, ErrCapabilityWorkerUnavailable
	}
	worker.nextID++
	id := worker.nextID
	worker.pending[id] = invocation
	worker.mu.Unlock()
	request := capabilityProcessFrame{Kind: "run", RequestID: id, Input: input, Determinism: control}
	if err := worker.writer.write(request); err != nil {
		worker.stop()
		return nil, ErrCapabilityWorkerUnavailable
	}
	return worker.await(ctx, id, invocation)
}

// admit bounds waiting callers and reserves one interpreter slot in the shared child.
func (worker *persistentCapabilityWorker) admit(ctx context.Context, limit int) error {
	select {
	case worker.queue <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-worker.closed:
		return ErrCapabilityWorkerUnavailable
	default:
		// A bounded waiting room prevents an overloaded family from accumulating HTTP goroutines.
		return ErrCapabilityWorkerOverloaded
	}
	// A separate slot gate lets a plan change take effect without replacing the app process.
	if err := worker.reserveSlot(ctx, limit); err != nil {
		<-worker.queue
		return err
	}
	return nil
}

// reserveSlot waits for the plan's per-app ceiling within the fixed process safety capacity.
func (worker *persistentCapabilityWorker) reserveSlot(ctx context.Context, limit int) error {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	stopWake := context.AfterFunc(ctx, func() {
		worker.mu.Lock()
		worker.slotCond.Broadcast()
		worker.mu.Unlock()
	})
	defer stopWake()
	for {
		// A canceled or dead request must leave the bounded queue without claiming an interpreter.
		if err := ctx.Err(); err != nil {
			return err
		}
		if worker.isClosedLocked() {
			return ErrCapabilityWorkerUnavailable
		}
		// The mutex makes occupancy checks and slot claims atomic across competing requests.
		if len(worker.slots) < limit {
			worker.slots <- struct{}{}
			return nil
		}
		worker.slotCond.Wait()
	}
}

// releaseSlotLocked wakes queued calls only after a completed interpreter frees capacity.
func (worker *persistentCapabilityWorker) releaseSlotLocked() {
	<-worker.slots
	worker.slotCond.Broadcast()
}

// await returns one invocation's result without taking a slot from unrelated requests.
func (worker *persistentCapabilityWorker) await(ctx context.Context, id uint64, invocation *capabilityInvocation) (json.RawMessage, error) {
	select {
	case result := <-invocation.result:
		return result.output, result.err
	case <-ctx.Done():
		// An abandoned HTTP request releases its slot only when the child finishes canceling.
		_ = worker.writer.write(capabilityProcessFrame{Kind: "cancel", RequestID: id})
		return nil, ctx.Err()
	case <-worker.closed:
		return nil, ErrCapabilityWorkerUnavailable
	}
}

// readLoop validates the load acknowledgement and then dispatches one shared IPC stream.
func (worker *persistentCapabilityWorker) readLoop(scanner *bufio.Scanner, ready chan<- error) {
	if !scanner.Scan() {
		ready <- ErrCapabilityWorkerUnavailable
		worker.stop()
		return
	}
	loaded, err := decodeCapabilityFrame(scanner.Bytes())
	// No invocation may start unless the child acknowledged its exact resident bundle.
	if err != nil || loaded.Kind != "loaded" || loaded.RequestID != 0 {
		ready <- ErrCapabilityWorkerUnavailable
		worker.stop()
		return
	}
	ready <- nil
	for scanner.Scan() {
		frame, err := decodeCapabilityFrame(scanner.Bytes())
		// A malformed worker frame invalidates the entire confined process.
		if err != nil || worker.handleFrame(frame) != nil {
			worker.stop()
			return
		}
	}
	worker.stop()
}

// handleFrame routes a child call or result only to its originating invocation.
func (worker *persistentCapabilityWorker) handleFrame(frame capabilityProcessFrame) error {
	worker.mu.Lock()
	invocation := worker.pending[frame.RequestID]
	worker.mu.Unlock()
	// Unknown request IDs cannot acquire an unrelated execution's host authority.
	if invocation == nil || frame.RequestID == 0 {
		return errors.New("capability worker protocol failed")
	}
	output, done, err := handleCapabilityProcessFrame(invocation.ctx, worker.writer, "run", invocation.host, frame, invocation.ids, invocation.ordinals, invocation.parallel)
	// An invalid host call is a process protocol violation; an authored done error belongs only to this request.
	if err != nil && !done {
		return err
	}
	if done {
		worker.finish(frame.RequestID, capabilityInvocationResult{output: output, err: err})
	}
	return nil
}

// finish releases capacity exactly once after the child's final frame.
func (worker *persistentCapabilityWorker) finish(id uint64, result capabilityInvocationResult) {
	worker.mu.Lock()
	invocation := worker.pending[id]
	delete(worker.pending, id)
	// A late frame after process shutdown has no slot left to release.
	if invocation != nil {
		worker.releaseSlotLocked()
	}
	worker.mu.Unlock()
	// Canceled callers may no longer be listening, so the result channel is buffered.
	if invocation != nil {
		invocation.result <- result
	}
}

// active reports interpreter slots that have not yet emitted a final frame.
func (worker *persistentCapabilityWorker) active() int {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	return len(worker.pending)
}

// isClosed lets the manager replace a child that exhausted its cumulative CPU budget.
func (worker *persistentCapabilityWorker) isClosed() bool {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	return worker.isClosedLocked()
}

// isClosedLocked checks process state while the pending map lock is held.
func (worker *persistentCapabilityWorker) isClosedLocked() bool {
	select {
	case <-worker.closed:
		return true
	default:
		return false
	}
}

// stop terminates the child and fails every pending execution without duplicating cleanup.
func (worker *persistentCapabilityWorker) stop() {
	worker.once.Do(func() {
		worker.mu.Lock()
		close(worker.closed)
		pending := worker.pending
		worker.pending = make(map[uint64]*capabilityInvocation)
		// Process shutdown must wake every queued plan-slot waiter and release admitted slots once.
		for range pending {
			worker.releaseSlotLocked()
		}
		worker.slotCond.Broadcast()
		worker.mu.Unlock()
		for _, invocation := range pending {
			invocation.result <- capabilityInvocationResult{err: ErrCapabilityWorkerUnavailable}
		}
		_ = worker.stdin.Close()
		_ = worker.command.Process.Kill()
		_ = worker.command.Wait()
		worker.cleanup()
	})
}
