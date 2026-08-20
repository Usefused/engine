package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
)

const (
	defaultConnectedAuthRefreshWorkers       = 4
	maxConnectedAuthRefreshWorkers           = 64
	defaultConnectedAuthRefreshInterval      = time.Hour
	defaultConnectedAuthRefreshLookahead     = 70 * time.Minute
	defaultConnectedAuthRefreshBatchSize     = 100
	defaultConnectedAuthRefreshLeaseDuration = 15 * time.Minute
	defaultConnectedAuthRefreshJobJitter     = 250 * time.Millisecond
)

var errConnectedAuthRefreshPassRunning = errors.New("connected auth refresh pass is already running")

// ConnectedAuthRefreshStore claims bounded pages while keeping provider work
// outside the serialized background database discovery gate.
type ConnectedAuthRefreshStore interface {
	ClaimAuthConnectionsForRefresh(ctx context.Context, cutoff, passStartedAt, now, leaseExpiresAt time.Time, limit int) ([]store.AuthConnectionRefreshClaim, error)
}

// ConnectedAuthRefreshExecutor performs one claimed refresh and owns all
// provider-specific token exchange and atomic completion behavior.
type ConnectedAuthRefreshExecutor interface {
	RefreshClaimedConnection(context.Context, store.AuthConnectionRefreshClaim) (sandbox.AuthRefreshResult, error)
}

// ConnectedAuthRefreshTicker is the minimum timer contract needed to test the
// scheduler without waiting for a wall-clock hour.
type ConnectedAuthRefreshTicker interface {
	Chan() <-chan time.Time
	Stop()
}

// ConnectedAuthRefreshOptions configures scheduling and bounded concurrency;
// clock hooks exist only to keep timing tests deterministic.
type ConnectedAuthRefreshOptions struct {
	WorkerCount   int
	Interval      time.Duration
	Lookahead     time.Duration
	BatchSize     int
	QueueSize     int
	LeaseDuration time.Duration
	MaxJobJitter  time.Duration
	Now           func() time.Time
	NewTicker     func(time.Duration) ConnectedAuthRefreshTicker
}

// ConnectedAuthRefreshSummary contains only bounded, secret-free outcomes for
// one pass and can safely be used by logs, tests, and future metrics.
type ConnectedAuthRefreshSummary struct {
	Claimed             int
	Attempted           int
	Refreshed           int
	NotDue              int
	LeaseContended      int
	TransientFailures   int
	ReconnectRequired   int
	ContractUnavailable int
	ExecutorFailures    int
	Skipped             bool
}

// ConnectedAuthRefreshWorker schedules one immediate pass and subsequent
// hourly passes, each backed by a fixed-size goroutine pool.
type ConnectedAuthRefreshWorker struct {
	store     ConnectedAuthRefreshStore
	executor  ConnectedAuthRefreshExecutor
	opts      ConnectedAuthRefreshOptions
	passGate  chan struct{}
	lifecycle sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	started   bool
	stopAsked bool
	startOnce sync.Once
	stopOnce  sync.Once
}

// systemConnectedAuthRefreshTicker adapts time.Ticker to the testable worker
// timer interface.
type systemConnectedAuthRefreshTicker struct {
	ticker *time.Ticker
}

// Chan exposes only ticker delivery and prevents callers from resetting the
// scheduler behind the worker's back.
func (t *systemConnectedAuthRefreshTicker) Chan() <-chan time.Time {
	return t.ticker.C
}

// Stop releases the runtime ticker when the worker shuts down.
func (t *systemConnectedAuthRefreshTicker) Stop() {
	t.ticker.Stop()
}

// NewConnectedAuthRefreshWorker validates concurrency and constructs a worker
// without starting any background goroutines.
func NewConnectedAuthRefreshWorker(refreshStore ConnectedAuthRefreshStore, executor ConnectedAuthRefreshExecutor, options ConnectedAuthRefreshOptions) (*ConnectedAuthRefreshWorker, error) {
	normalized, err := normalizeConnectedAuthRefreshOptions(options)
	if err != nil {
		return nil, err
	}
	if refreshStore == nil || executor == nil {
		// Why: a partially wired refresh worker would appear healthy while
		// silently allowing grants to expire.
		return nil, errors.New("connected auth refresh store and executor are required")
	}
	return &ConnectedAuthRefreshWorker{
		store: refreshStore, executor: executor, opts: normalized,
		passGate: make(chan struct{}, 1), done: make(chan struct{}),
	}, nil
}

// normalizeConnectedAuthRefreshOptions supplies safe operational defaults and
// rejects unsupported concurrency rather than silently changing it.
func normalizeConnectedAuthRefreshOptions(options ConnectedAuthRefreshOptions) (ConnectedAuthRefreshOptions, error) {
	// Why: a zero programmatic value means use the documented default; explicit
	// deployment configuration is validated before this constructor is called.
	if options.WorkerCount == 0 {
		options.WorkerCount = defaultConnectedAuthRefreshWorkers
	}
	if options.WorkerCount < 1 || options.WorkerCount > maxConnectedAuthRefreshWorkers {
		// Why: excessive concurrency can overload provider token endpoints;
		// negative concurrency cannot create a meaningful pool.
		return ConnectedAuthRefreshOptions{}, fmt.Errorf("connected auth refresh workers must be between 1 and %d, got %d", maxConnectedAuthRefreshWorkers, options.WorkerCount)
	}
	options = normalizeConnectedAuthRefreshRuntimeOptions(options)
	options = normalizeConnectedAuthRefreshHooks(options)
	return options, nil
}

// normalizeConnectedAuthRefreshRuntimeOptions supplies bounded scheduling,
// page, queue, lease, and jitter defaults independently of pool validation.
func normalizeConnectedAuthRefreshRuntimeOptions(options ConnectedAuthRefreshOptions) ConnectedAuthRefreshOptions {
	// Why: non-positive timing and page values cannot form a useful managed
	// refresh pass, so programmatic callers inherit production-safe defaults.
	if options.Interval <= 0 {
		options.Interval = defaultConnectedAuthRefreshInterval
	}
	if options.Lookahead <= 0 {
		options.Lookahead = defaultConnectedAuthRefreshLookahead
	}
	if options.BatchSize <= 0 {
		options.BatchSize = defaultConnectedAuthRefreshBatchSize
	}
	if options.QueueSize <= 0 {
		// Why: twice the worker count keeps the pool fed without accumulating
		// an unbounded number of leased-but-not-started refresh jobs.
		options.QueueSize = options.WorkerCount * 2
	}
	maxClaimPage := options.WorkerCount + options.QueueSize
	if options.BatchSize > maxClaimPage {
		// Why: database claims acquire a rotating-token lease before queue
		// admission, so one page must fit the active pool plus its bounded queue.
		options.BatchSize = maxClaimPage
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = defaultConnectedAuthRefreshLeaseDuration
	}
	return options
}

// normalizeConnectedAuthRefreshHooks supplies production time sources while
// retaining explicit no-jitter behavior for deterministic tests.
func normalizeConnectedAuthRefreshHooks(options ConnectedAuthRefreshOptions) ConnectedAuthRefreshOptions {
	// Why: ordinary callers get small production jitter and real time sources;
	// explicit hooks keep scheduler tests deterministic without sleeping.
	if options.MaxJobJitter == 0 {
		options.MaxJobJitter = defaultConnectedAuthRefreshJobJitter
	}
	if options.MaxJobJitter < 0 {
		// Why: tests and controlled deployments need an explicit way to turn
		// jitter off without changing the production default.
		options.MaxJobJitter = 0
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewTicker == nil {
		options.NewTicker = newSystemConnectedAuthRefreshTicker
	}
	return options
}

// newSystemConnectedAuthRefreshTicker constructs the production hourly timer.
func newSystemConnectedAuthRefreshTicker(interval time.Duration) ConnectedAuthRefreshTicker {
	return &systemConnectedAuthRefreshTicker{ticker: time.NewTicker(interval)}
}

// Start launches the startup pass and periodic scheduler exactly once.
func (w *ConnectedAuthRefreshWorker) Start(ctx context.Context) {
	if w == nil {
		return
	}
	// Why: Engine bootstrap may compose workers through multiple optional
	// paths, but only one scheduler may own this worker instance.
	w.startOnce.Do(func() {
		workerCtx, cancel := context.WithCancel(ctx)
		w.lifecycle.Lock()
		w.cancel = cancel
		w.started = true
		stopAsked := w.stopAsked
		w.lifecycle.Unlock()
		if stopAsked {
			// Why: a concurrent pre-start Stop must win over later scheduler launch.
			cancel()
		}
		go w.run(workerCtx)
	})
}

// Stop cancels the dispatcher and waits only as long as the caller's bounded
// shutdown context permits.
func (w *ConnectedAuthRefreshWorker) Stop(ctx context.Context) {
	if w == nil {
		return
	}
	// Why: repeated shutdown paths must converge on one cancellation signal.
	w.stopOnce.Do(func() {
		w.lifecycle.Lock()
		w.stopAsked = true
		cancel := w.cancel
		w.lifecycle.Unlock()
		if cancel != nil {
			cancel()
		}
	})
	w.lifecycle.Lock()
	started := w.started
	w.lifecycle.Unlock()
	if !started {
		return
	}
	select {
	case <-w.done:
	case <-ctx.Done():
	}
}

// RunOnce drains every connection due within the fixed pass lookahead using a
// bounded pool; the supplied time anchors pass-level exclusion semantics.
func (w *ConnectedAuthRefreshWorker) RunOnce(ctx context.Context, passStartedAt time.Time) (ConnectedAuthRefreshSummary, error) {
	if w == nil {
		return ConnectedAuthRefreshSummary{}, errors.New("connected auth refresh worker is nil")
	}
	if !w.acquirePass() {
		// Why: a slow provider pass must coalesce ticks and manual triggers
		// rather than multiply refresh traffic against the same grants.
		return ConnectedAuthRefreshSummary{Skipped: true}, errConnectedAuthRefreshPassRunning
	}
	defer w.releasePass()
	return w.runPass(ctx, passStartedAt.UTC())
}

// acquirePass performs a non-blocking single-flight admission check.
func (w *ConnectedAuthRefreshWorker) acquirePass() bool {
	select {
	case w.passGate <- struct{}{}:
		return true
	default:
		return false
	}
}

// releasePass makes the next scheduled or manual pass eligible to run.
func (w *ConnectedAuthRefreshWorker) releasePass() {
	<-w.passGate
}

// run owns scheduler sequencing, which inherently prevents periodic passes
// from overlapping even when one provider pass lasts beyond an interval.
func (w *ConnectedAuthRefreshWorker) run(ctx context.Context) {
	defer close(w.done)
	ticker := w.opts.NewTicker(w.opts.Interval)
	defer ticker.Stop()
	// Why: arming the ticker before the startup pass preserves the hourly
	// cadence even when the first provider batch itself runs for over an hour.
	w.runScheduledPass(ctx, "startup")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
			// Why: collapsing any ticks already buffered during a slow pass
			// prevents a burst of back-to-back provider scans afterward.
			drainConnectedAuthRefreshTicks(ticker.Chan())
			w.runScheduledPass(ctx, "periodic")
		}
	}
}

// drainConnectedAuthRefreshTicks discards already-buffered timer events while
// leaving future ticks available to schedule the next pass.
func drainConnectedAuthRefreshTicks(ticks <-chan time.Time) {
	for {
		select {
		case <-ticks:
			continue
		default:
			return
		}
	}
}

// runScheduledPass records one safe aggregate log and never lets a failed pass
// terminate the long-running scheduler.
func (w *ConnectedAuthRefreshWorker) runScheduledPass(ctx context.Context, trigger string) {
	summary, err := w.RunOnce(ctx, w.opts.Now().UTC())
	if err != nil {
		// Why: log a stable code instead of an error object that could contain
		// a provider response or other credential-adjacent text.
		slog.WarnContext(ctx, "Connected auth refresh pass failed",
			slog.String("trigger", trigger), slog.String("error_code", "connected_auth_refresh_pass_failed"))
		return
	}
	slog.InfoContext(ctx, "Connected auth refresh pass completed",
		slog.String("trigger", trigger),
		slog.Int("claimed", summary.Claimed),
		slog.Int("attempted", summary.Attempted),
		slog.Int("refreshed", summary.Refreshed),
		slog.Int("not_due", summary.NotDue),
		slog.Int("lease_contended", summary.LeaseContended),
		slog.Int("transient_failure", summary.TransientFailures),
		slog.Int("reconnect_required", summary.ReconnectRequired),
		slog.Int("contract_unavailable", summary.ContractUnavailable),
		slog.Int("executor_failure", summary.ExecutorFailures))
}

// runPass starts a fixed-size worker pool, drains claimed pages through its
// bounded queue, and then joins every goroutine before returning.
func (w *ConnectedAuthRefreshWorker) runPass(ctx context.Context, passStartedAt time.Time) (ConnectedAuthRefreshSummary, error) {
	jobs := make(chan store.AuthConnectionRefreshClaim, w.opts.QueueSize)
	results := make(chan ConnectedAuthRefreshSummary, w.opts.WorkerCount)
	var workers sync.WaitGroup
	w.startPool(ctx, jobs, results, &workers)

	claimed, dispatchErr := w.dispatchClaims(ctx, passStartedAt, jobs)
	close(jobs)
	workers.Wait()
	close(results)

	summary := collectConnectedAuthRefreshResults(results)
	summary.Claimed = claimed
	return summary, dispatchErr
}

// startPool launches exactly the configured number of refresh goroutines.
func (w *ConnectedAuthRefreshWorker) startPool(ctx context.Context, jobs <-chan store.AuthConnectionRefreshClaim, results chan<- ConnectedAuthRefreshSummary, workers *sync.WaitGroup) {
	workers.Add(w.opts.WorkerCount)
	for range w.opts.WorkerCount {
		go func() {
			defer workers.Done()
			results <- w.consumeClaims(ctx, jobs)
		}()
	}
}

// dispatchClaims claims and feeds every due page while retaining one immutable
// pass start so completed or deferred rows cannot loop back into this pass.
func (w *ConnectedAuthRefreshWorker) dispatchClaims(ctx context.Context, passStartedAt time.Time, jobs chan<- store.AuthConnectionRefreshClaim) (int, error) {
	cutoff := passStartedAt.Add(w.opts.Lookahead)
	seen := make(map[string]struct{})
	claimed := 0
	for {
		claims, err := w.claimPage(ctx, cutoff, passStartedAt)
		if err != nil {
			// Why: discovery failure ends only this pass; the scheduler will retry
			// on its next tick without terminating the long-running worker.
			return claimed, err
		}
		if len(claims) == 0 {
			// Why: the claim query is authoritative for whether the pass drained.
			return claimed, nil
		}
		queued, err := enqueueConnectedAuthRefreshClaims(ctx, jobs, claims, seen)
		claimed += queued
		if err != nil {
			return claimed, err
		}
		if queued == 0 || len(claims) < w.opts.BatchSize {
			// Why: an all-duplicate page indicates broken claim exclusion and
			// must stop rather than spin; a short page proves the due set drained.
			return claimed, nil
		}
	}
}

// claimPage serializes only the short database claim and gives each page a
// fresh lease window before any provider network call begins.
func (w *ConnectedAuthRefreshWorker) claimPage(ctx context.Context, cutoff, passStartedAt time.Time) ([]store.AuthConnectionRefreshClaim, error) {
	now := w.opts.Now().UTC()
	claims, err := w.store.ClaimAuthConnectionsForRefresh(ctx, cutoff, passStartedAt, now, now.Add(w.opts.LeaseDuration), w.opts.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("claim connected auth refresh page: %w", err)
	}
	return claims, nil
}

// enqueueConnectedAuthRefreshClaims filters defensive duplicates and applies
// bounded backpressure while the pool performs provider requests.
func enqueueConnectedAuthRefreshClaims(ctx context.Context, jobs chan<- store.AuthConnectionRefreshClaim, claims []store.AuthConnectionRefreshClaim, seen map[string]struct{}) (int, error) {
	queued := 0
	for _, claim := range claims {
		key := claim.Connection.ID.String()
		if _, duplicate := seen[key]; duplicate {
			// Why: defensive filtering prevents a broken store implementation
			// from refreshing one rotating token twice in a single pass.
			continue
		}
		seen[key] = struct{}{}
		select {
		case jobs <- claim:
			queued++
		case <-ctx.Done():
			return queued, ctx.Err()
		}
	}
	return queued, nil
}

// consumeClaims processes independent jobs until the dispatcher closes the
// queue; one executor failure changes only this worker's bounded counters.
func (w *ConnectedAuthRefreshWorker) consumeClaims(ctx context.Context, jobs <-chan store.AuthConnectionRefreshClaim) ConnectedAuthRefreshSummary {
	summary := ConnectedAuthRefreshSummary{}
	for claim := range jobs {
		if err := w.waitForJobJitter(ctx); err != nil {
			// Why: cancellation leaves the durable lease to expire naturally
			// instead of starting new provider traffic during shutdown.
			continue
		}
		summary.Attempted++
		result, err := w.executor.RefreshClaimedConnection(ctx, claim)
		if result.Outcome != "" {
			// Why: coordinator errors are sanitized control signals, while the
			// accompanying bounded outcome remains authoritative for telemetry.
			classifyConnectedAuthRefreshOutcome(&summary, result.Outcome)
			continue
		}
		if err != nil {
			// Why: executor failures belong to one connection and cannot stop
			// other jobs; a missing bounded outcome is the only generic failure.
			summary.ExecutorFailures++
			continue
		}
		classifyConnectedAuthRefreshOutcome(&summary, result.Outcome)
	}
	return summary
}

// waitForJobJitter spreads simultaneous token exchanges across a small bounded
// interval and remains immediately cancellable during Engine shutdown.
func (w *ConnectedAuthRefreshWorker) waitForJobJitter(ctx context.Context) error {
	if w.opts.MaxJobJitter <= 0 {
		return nil
	}
	delay := time.Duration(rand.Int64N(int64(w.opts.MaxJobJitter) + 1))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// classifyConnectedAuthRefreshOutcome increments only the documented bounded
// outcome set and treats unknown values as executor failures.
func classifyConnectedAuthRefreshOutcome(summary *ConnectedAuthRefreshSummary, outcome sandbox.AuthRefreshOutcome) {
	switch outcome {
	case sandbox.AuthRefreshOutcomeRefreshed:
		summary.Refreshed++
	case sandbox.AuthRefreshOutcomeNotDue:
		summary.NotDue++
	case sandbox.AuthRefreshOutcomeLeaseContended:
		summary.LeaseContended++
	case sandbox.AuthRefreshOutcomeTransientFailure:
		summary.TransientFailures++
	case sandbox.AuthRefreshOutcomeReconnectRequired:
		summary.ReconnectRequired++
	case sandbox.AuthRefreshOutcomeContractUnavailable:
		summary.ContractUnavailable++
	default:
		// Why: an unrecognized result must remain visible without creating an
		// unbounded metric or log label from provider-controlled text.
		summary.ExecutorFailures++
	}
}

// collectConnectedAuthRefreshResults merges one fixed-size local summary per
// goroutine without locks on the provider hot path.
func collectConnectedAuthRefreshResults(results <-chan ConnectedAuthRefreshSummary) ConnectedAuthRefreshSummary {
	total := ConnectedAuthRefreshSummary{}
	for result := range results {
		total.Attempted += result.Attempted
		total.Refreshed += result.Refreshed
		total.NotDue += result.NotDue
		total.LeaseContended += result.LeaseContended
		total.TransientFailures += result.TransientFailures
		total.ReconnectRequired += result.ReconnectRequired
		total.ContractUnavailable += result.ContractUnavailable
		total.ExecutorFailures += result.ExecutorFailures
	}
	return total
}
