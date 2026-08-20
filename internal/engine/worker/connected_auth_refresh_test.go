package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// connectedAuthRefreshStoreStub records discovery timing and returns scripted
// pages so worker scheduling tests never require PostgreSQL.
type connectedAuthRefreshStoreStub struct {
	mu        sync.Mutex
	pages     [][]store.AuthConnectionRefreshClaim
	pageIndex int
	always    []store.AuthConnectionRefreshClaim
	calls     int
	cutoffs   []time.Time
	passTimes []time.Time
	limits    []int
	started   chan struct{}
	startOnce sync.Once
}

// ClaimAuthConnectionsForRefresh implements the worker's narrow claim store
// while preserving copies of scripted claim pages for assertions.
func (s *connectedAuthRefreshStoreStub) ClaimAuthConnectionsForRefresh(_ context.Context, cutoff, passStartedAt, _, _ time.Time, limit int) ([]store.AuthConnectionRefreshClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.cutoffs = append(s.cutoffs, cutoff)
	s.passTimes = append(s.passTimes, passStartedAt)
	s.limits = append(s.limits, limit)
	if s.started != nil {
		s.startOnce.Do(func() { close(s.started) })
	}
	if s.always != nil {
		return append([]store.AuthConnectionRefreshClaim(nil), s.always...), nil
	}
	if s.pageIndex >= len(s.pages) {
		return nil, nil
	}
	page := append([]store.AuthConnectionRefreshClaim(nil), s.pages[s.pageIndex]...)
	s.pageIndex++
	return page, nil
}

// TestConnectedAuthRefreshWorkerBoundsClaimPageByPoolCapacity proves a
// one-worker deployment never leases the default hundred-row page at once.
func TestConnectedAuthRefreshWorkerBoundsClaimPageByPoolCapacity(t *testing.T) {
	refreshStore := &connectedAuthRefreshStoreStub{}
	executor := &connectedAuthRefreshExecutorStub{}
	refreshWorker := newConnectedAuthRefreshTestWorker(t, refreshStore, executor, ConnectedAuthRefreshOptions{
		WorkerCount: 1, BatchSize: 100, QueueSize: 2,
	})

	if _, err := refreshWorker.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	refreshStore.mu.Lock()
	defer refreshStore.mu.Unlock()
	if len(refreshStore.limits) != 1 || refreshStore.limits[0] != 3 {
		t.Fatalf("claim limits = %v, want [3]", refreshStore.limits)
	}
}

// connectedAuthRefreshExecutorStub measures concurrency, supports one injected
// failure, and can pause a selected call for lifecycle tests.
type connectedAuthRefreshExecutorStub struct {
	mu            sync.Mutex
	calls         int
	active        int
	maxActive     int
	failID        uuid.UUID
	transientID   uuid.UUID
	blockCall     int
	block         chan struct{}
	started       chan int
	contextBlocks bool
}

// RefreshClaimedConnection implements the coordinator boundary without ever
// constructing or exposing token material.
func (e *connectedAuthRefreshExecutorStub) RefreshClaimedConnection(ctx context.Context, claim store.AuthConnectionRefreshClaim) (sandbox.AuthRefreshResult, error) {
	call := e.beginCall()
	defer e.endCall()
	if e.started != nil {
		e.started <- call
	}
	if e.contextBlocks {
		<-ctx.Done()
		return sandbox.AuthRefreshResult{}, ctx.Err()
	}
	if e.block != nil && (e.blockCall == 0 || e.blockCall == call) {
		select {
		case <-e.block:
		case <-ctx.Done():
			return sandbox.AuthRefreshResult{}, ctx.Err()
		}
	}
	if claim.Connection.ID == e.failID {
		return sandbox.AuthRefreshResult{}, errors.New("sanitized executor failure")
	}
	if claim.Connection.ID == e.transientID {
		return sandbox.AuthRefreshResult{Outcome: sandbox.AuthRefreshOutcomeTransientFailure, FailureCode: "provider_refresh_failed"}, sandbox.ErrAuthRefreshFailed
	}
	return sandbox.AuthRefreshResult{Outcome: sandbox.AuthRefreshOutcomeRefreshed}, nil
}

// beginCall records one executor admission and returns its stable ordinal.
func (e *connectedAuthRefreshExecutorStub) beginCall() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	e.active++
	if e.active > e.maxActive {
		e.maxActive = e.active
	}
	return e.calls
}

// endCall records executor completion for maximum-concurrency assertions.
func (e *connectedAuthRefreshExecutorStub) endCall() {
	e.mu.Lock()
	e.active--
	e.mu.Unlock()
}

// snapshot returns race-safe executor totals for test assertions.
func (e *connectedAuthRefreshExecutorStub) snapshot() (calls, maxActive int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls, e.maxActive
}

// connectedAuthRefreshTickerStub provides manually triggered scheduler ticks.
type connectedAuthRefreshTickerStub struct {
	ticks   chan time.Time
	stopped chan struct{}
	once    sync.Once
}

// Chan returns the manually controlled tick stream.
func (t *connectedAuthRefreshTickerStub) Chan() <-chan time.Time {
	return t.ticks
}

// Stop records ticker disposal without closing the producer-owned tick stream.
func (t *connectedAuthRefreshTickerStub) Stop() {
	t.once.Do(func() { close(t.stopped) })
}

// TestConnectedAuthRefreshWorkerStartsWithImmediatePass proves startup does not
// wait for the first one-hour scheduler tick.
func TestConnectedAuthRefreshWorkerStartsWithImmediatePass(t *testing.T) {
	started := make(chan struct{})
	refreshStore := &connectedAuthRefreshStoreStub{started: started}
	executor := &connectedAuthRefreshExecutorStub{}
	refreshWorker := newConnectedAuthRefreshTestWorker(t, refreshStore, executor, ConnectedAuthRefreshOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refreshWorker.Start(ctx)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup refresh pass did not begin immediately")
	}
	stopConnectedAuthRefreshTestWorker(t, refreshWorker)
}

// TestConnectedAuthRefreshWorkerConcurrentStartStop proves shutdown cannot
// miss a scheduler whose Start races with an earlier stop request.
func TestConnectedAuthRefreshWorkerConcurrentStartStop(t *testing.T) {
	for range 100 {
		refreshStore := &connectedAuthRefreshStoreStub{}
		executor := &connectedAuthRefreshExecutorStub{}
		refreshWorker := newConnectedAuthRefreshTestWorker(t, refreshStore, executor, ConnectedAuthRefreshOptions{})
		start := make(chan struct{})
		var calls sync.WaitGroup
		calls.Add(2)
		go func() {
			defer calls.Done()
			<-start
			refreshWorker.Start(context.Background())
		}()
		go func() {
			defer calls.Done()
			<-start
			stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			refreshWorker.Stop(stopCtx)
		}()
		close(start)
		calls.Wait()
		stopConnectedAuthRefreshTestWorker(t, refreshWorker)
	}
}

// TestConnectedAuthRefreshWorkerHonorsConfiguredConcurrency proves the pool
// never exceeds the exact worker count selected by the operator.
func TestConnectedAuthRefreshWorkerHonorsConfiguredConcurrency(t *testing.T) {
	claims := connectedAuthRefreshTestClaims(12)
	refreshStore := &connectedAuthRefreshStoreStub{pages: [][]store.AuthConnectionRefreshClaim{claims}}
	block := make(chan struct{})
	started := make(chan int, len(claims))
	executor := &connectedAuthRefreshExecutorStub{block: block, started: started}
	refreshWorker := newConnectedAuthRefreshTestWorker(t, refreshStore, executor, ConnectedAuthRefreshOptions{
		WorkerCount: 3, BatchSize: 20, QueueSize: 6,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = refreshWorker.RunOnce(context.Background(), time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	}()

	waitForConnectedAuthRefreshCalls(t, started, 3)
	_, maxActive := executor.snapshot()
	if maxActive != 3 {
		t.Fatalf("maximum executor concurrency = %d, want 3", maxActive)
	}
	close(block)
	waitForConnectedAuthRefreshDone(t, done)
	_, maxActive = executor.snapshot()
	if maxActive > 3 {
		t.Fatalf("maximum executor concurrency exceeded configured pool: %d", maxActive)
	}
}

// TestConnectedAuthRefreshWorkerDrainsEveryPage proves one pass continues
// beyond the first database batch until discovery returns a short page.
func TestConnectedAuthRefreshWorkerDrainsEveryPage(t *testing.T) {
	claims := connectedAuthRefreshTestClaims(5)
	refreshStore := &connectedAuthRefreshStoreStub{pages: [][]store.AuthConnectionRefreshClaim{
		claims[:2], claims[2:4], claims[4:],
	}}
	executor := &connectedAuthRefreshExecutorStub{}
	refreshWorker := newConnectedAuthRefreshTestWorker(t, refreshStore, executor, ConnectedAuthRefreshOptions{
		WorkerCount: 2, BatchSize: 2, QueueSize: 4,
	})
	passStartedAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	summary, err := refreshWorker.RunOnce(context.Background(), passStartedAt)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if summary.Claimed != 5 || summary.Refreshed != 5 || summary.Attempted != 5 {
		t.Fatalf("summary = %+v, want five successful refreshes", summary)
	}
	assertConnectedAuthRefreshPassTimes(t, refreshStore, passStartedAt, 3)
}

// TestConnectedAuthRefreshWorkerIsolatesJobFailure proves one connection error
// does not terminate the pool or prevent unrelated grants from refreshing.
func TestConnectedAuthRefreshWorkerIsolatesJobFailure(t *testing.T) {
	claims := connectedAuthRefreshTestClaims(4)
	refreshStore := &connectedAuthRefreshStoreStub{pages: [][]store.AuthConnectionRefreshClaim{claims}}
	executor := &connectedAuthRefreshExecutorStub{failID: claims[1].Connection.ID}
	refreshWorker := newConnectedAuthRefreshTestWorker(t, refreshStore, executor, ConnectedAuthRefreshOptions{WorkerCount: 2, BatchSize: 10})

	summary, err := refreshWorker.RunOnce(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if summary.Refreshed != 3 || summary.ExecutorFailures != 1 || summary.Attempted != 4 {
		t.Fatalf("failure-isolated summary = %+v", summary)
	}
}

// TestConnectedAuthRefreshWorkerCountsBoundedFailureOutcome proves sanitized
// coordinator errors retain their authoritative low-cardinality result.
func TestConnectedAuthRefreshWorkerCountsBoundedFailureOutcome(t *testing.T) {
	claims := connectedAuthRefreshTestClaims(3)
	refreshStore := &connectedAuthRefreshStoreStub{pages: [][]store.AuthConnectionRefreshClaim{claims}}
	executor := &connectedAuthRefreshExecutorStub{transientID: claims[1].Connection.ID}
	refreshWorker := newConnectedAuthRefreshTestWorker(t, refreshStore, executor, ConnectedAuthRefreshOptions{WorkerCount: 2, BatchSize: 10})

	summary, err := refreshWorker.RunOnce(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if summary.Refreshed != 2 || summary.TransientFailures != 1 || summary.ExecutorFailures != 0 {
		t.Fatalf("bounded failure summary = %+v", summary)
	}
}

// TestClassifyConnectedAuthRefreshCoordinationOutcomes proves benign race and
// freshness decisions remain distinct from refresh or executor failures.
func TestClassifyConnectedAuthRefreshCoordinationOutcomes(t *testing.T) {
	summary := ConnectedAuthRefreshSummary{}
	classifyConnectedAuthRefreshOutcome(&summary, sandbox.AuthRefreshOutcomeNotDue)
	classifyConnectedAuthRefreshOutcome(&summary, sandbox.AuthRefreshOutcomeLeaseContended)
	if summary.NotDue != 1 || summary.LeaseContended != 1 || summary.ExecutorFailures != 0 {
		t.Fatalf("coordination outcome summary = %+v", summary)
	}
}

// TestConnectedAuthRefreshWorkerCoalescesBufferedTicks proves a slow pass cannot
// create overlapping pools or one catch-up pass per missed scheduler tick.
func TestConnectedAuthRefreshWorkerCoalescesBufferedTicks(t *testing.T) {
	claim := connectedAuthRefreshTestClaims(1)
	refreshStore := &connectedAuthRefreshStoreStub{always: claim}
	ticker := &connectedAuthRefreshTickerStub{ticks: make(chan time.Time, 4), stopped: make(chan struct{})}
	block := make(chan struct{})
	started := make(chan int, 4)
	executor := &connectedAuthRefreshExecutorStub{blockCall: 2, block: block, started: started}
	refreshWorker := newConnectedAuthRefreshTestWorker(t, refreshStore, executor, ConnectedAuthRefreshOptions{
		WorkerCount: 1, BatchSize: 10, NewTicker: func(time.Duration) ConnectedAuthRefreshTicker { return ticker },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refreshWorker.Start(ctx)

	waitForConnectedAuthRefreshCalls(t, started, 1)
	ticker.ticks <- time.Now()
	waitForConnectedAuthRefreshCalls(t, started, 1)
	ticker.ticks <- time.Now()
	ticker.ticks <- time.Now()
	close(block)
	waitForConnectedAuthRefreshCalls(t, started, 1)
	time.Sleep(25 * time.Millisecond)
	calls, maxActive := executor.snapshot()
	if calls != 3 || maxActive != 1 {
		t.Fatalf("scheduler calls/max concurrency = %d/%d, want 3/1", calls, maxActive)
	}
	stopConnectedAuthRefreshTestWorker(t, refreshWorker)
}

// TestConnectedAuthRefreshWorkerCancellationStopsPool proves shutdown context
// cancellation reaches in-flight executor calls and joins the scheduler.
func TestConnectedAuthRefreshWorkerCancellationStopsPool(t *testing.T) {
	claim := connectedAuthRefreshTestClaims(1)
	refreshStore := &connectedAuthRefreshStoreStub{always: claim}
	started := make(chan int, 1)
	executor := &connectedAuthRefreshExecutorStub{started: started, contextBlocks: true}
	refreshWorker := newConnectedAuthRefreshTestWorker(t, refreshStore, executor, ConnectedAuthRefreshOptions{WorkerCount: 1, BatchSize: 10})
	refreshWorker.Start(context.Background())
	waitForConnectedAuthRefreshCalls(t, started, 1)

	stopConnectedAuthRefreshTestWorker(t, refreshWorker)
	select {
	case <-refreshWorker.done:
	default:
		t.Fatal("worker remained active after bounded cancellation")
	}
}

// TestConnectedAuthRefreshWorkerRejectsSamePassLoop proves duplicate claims from
// a broken store cannot spin forever or execute a connection twice.
func TestConnectedAuthRefreshWorkerRejectsSamePassLoop(t *testing.T) {
	claim := connectedAuthRefreshTestClaims(1)
	refreshStore := &connectedAuthRefreshStoreStub{always: claim}
	executor := &connectedAuthRefreshExecutorStub{}
	refreshWorker := newConnectedAuthRefreshTestWorker(t, refreshStore, executor, ConnectedAuthRefreshOptions{
		WorkerCount: 1, BatchSize: 1, QueueSize: 1,
	})

	summary, err := refreshWorker.RunOnce(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	calls, _ := executor.snapshot()
	if summary.Claimed != 1 || calls != 1 || refreshStore.calls != 2 {
		t.Fatalf("duplicate pass summary/calls/store calls = %+v/%d/%d", summary, calls, refreshStore.calls)
	}
}

// TestConnectedAuthRefreshWorkerRejectsInvalidWorkerCount protects direct
// constructor callers in addition to shared configuration validation.
func TestConnectedAuthRefreshWorkerRejectsInvalidWorkerCount(t *testing.T) {
	refreshStore := &connectedAuthRefreshStoreStub{}
	executor := &connectedAuthRefreshExecutorStub{}
	for _, workers := range []int{-1, 65} {
		if _, err := NewConnectedAuthRefreshWorker(refreshStore, executor, ConnectedAuthRefreshOptions{WorkerCount: workers}); err == nil {
			t.Fatalf("constructor accepted worker count %d", workers)
		}
	}
}

// newConnectedAuthRefreshTestWorker constructs a no-jitter worker and fails the
// current test immediately when its options are invalid.
func newConnectedAuthRefreshTestWorker(t *testing.T, refreshStore ConnectedAuthRefreshStore, executor ConnectedAuthRefreshExecutor, options ConnectedAuthRefreshOptions) *ConnectedAuthRefreshWorker {
	t.Helper()
	options.MaxJobJitter = -1
	refreshWorker, err := NewConnectedAuthRefreshWorker(refreshStore, executor, options)
	if err != nil {
		t.Fatalf("NewConnectedAuthRefreshWorker: %v", err)
	}
	return refreshWorker
}

// stopConnectedAuthRefreshTestWorker stops a scheduler within a deterministic
// one-second test deadline.
func stopConnectedAuthRefreshTestWorker(t *testing.T, refreshWorker *ConnectedAuthRefreshWorker) {
	t.Helper()
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	refreshWorker.Stop(stopCtx)
	if err := stopCtx.Err(); err != nil {
		t.Fatalf("worker stop exceeded deadline: %v", err)
	}
}

// waitForConnectedAuthRefreshCalls waits for an exact number of executor start
// signals with a bounded timeout.
func waitForConnectedAuthRefreshCalls(t *testing.T, started <-chan int, count int) {
	t.Helper()
	for range count {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for refresh executor call")
		}
	}
}

// waitForConnectedAuthRefreshDone waits for one asynchronous pass to join.
func waitForConnectedAuthRefreshDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refresh pass completion")
	}
}

// connectedAuthRefreshTestClaims builds non-secret connection identities and
// unique lease tokens for deterministic worker tests.
func connectedAuthRefreshTestClaims(count int) []store.AuthConnectionRefreshClaim {
	claims := make([]store.AuthConnectionRefreshClaim, count)
	for index := range claims {
		claims[index] = store.AuthConnectionRefreshClaim{
			Connection: store.AuthConnection{ID: uuid.New()},
			LeaseToken: uuid.New(),
		}
	}
	return claims
}

// assertConnectedAuthRefreshPassTimes verifies paging retained one immutable
// pass start and the documented seventy-minute lookahead.
func assertConnectedAuthRefreshPassTimes(t *testing.T, refreshStore *connectedAuthRefreshStoreStub, passStartedAt time.Time, calls int) {
	t.Helper()
	refreshStore.mu.Lock()
	defer refreshStore.mu.Unlock()
	if len(refreshStore.passTimes) != calls {
		t.Fatalf("claim calls = %d, want %d", len(refreshStore.passTimes), calls)
	}
	for index := range refreshStore.passTimes {
		if !refreshStore.passTimes[index].Equal(passStartedAt) {
			t.Fatalf("claim %d pass start = %s, want %s", index, refreshStore.passTimes[index], passStartedAt)
		}
		if !refreshStore.cutoffs[index].Equal(passStartedAt.Add(defaultConnectedAuthRefreshLookahead)) {
			t.Fatalf("claim %d cutoff = %s", index, refreshStore.cutoffs[index])
		}
	}
}
