package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const (
	defaultSDKGenerationFinalizerInterval = 2 * time.Second
	defaultSDKGenerationFinalizerTimeout  = 10 * time.Second
	defaultSDKGenerationFinalizerBatch    = 100
)

// SDKGenerationBuildStore exposes the durable pending rows and compare-and-swap
// transitions needed by the finalizer without granting broader store access.
type SDKGenerationBuildStore interface {
	ListPendingSDKGenerationBuilds(context.Context, uuid.UUID, int) ([]store.SDKGenerationBuild, error)
	CompleteSDKGeneration(context.Context, uuid.UUID, string, string) (bool, error)
	FailSDKGeneration(context.Context, uuid.UUID, string, string) (bool, error)
}

// SDKGenerationClient replays the exact retained request and returns the
// Registry's current durable job state.
type SDKGenerationClient interface {
	GenerateSDK(context.Context, models.SDKGenerationRequest) (models.SDKGenerationResult, error)
}

// SDKGenerationFinalizerOptions bounds polling and provides the runtime-cache
// invalidation hook used only by the successful activation CAS winner.
type SDKGenerationFinalizerOptions struct {
	Interval       time.Duration
	RequestTimeout time.Duration
	BatchSize      int
	OnActivated    func(context.Context, uuid.UUID)
}

// SDKGenerationFinalizer advances durable SDK builds independently from the
// short apply response and survives Engine restarts through startup polling.
type SDKGenerationFinalizer struct {
	store     SDKGenerationBuildStore
	client    SDKGenerationClient
	opts      SDKGenerationFinalizerOptions
	cancel    context.CancelFunc
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewSDKGenerationFinalizer constructs a non-running worker with normalized
// bounds so zero-value production options remain safe.
func NewSDKGenerationFinalizer(buildStore SDKGenerationBuildStore, client SDKGenerationClient, opts SDKGenerationFinalizerOptions) *SDKGenerationFinalizer {
	return &SDKGenerationFinalizer{
		store: buildStore, client: client, opts: normalizeSDKGenerationFinalizerOptions(opts), done: make(chan struct{}),
	}
}

// normalizeSDKGenerationFinalizerOptions applies finite defaults and caps one
// database page at the shared package-work ceiling.
func normalizeSDKGenerationFinalizerOptions(opts SDKGenerationFinalizerOptions) SDKGenerationFinalizerOptions {
	// Two-second polling removes request-path latency while avoiding a hot database loop.
	if opts.Interval <= 0 {
		opts.Interval = defaultSDKGenerationFinalizerInterval
	}
	// Every Registry replay must finish before the next ordinary poll window can accumulate.
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultSDKGenerationFinalizerTimeout
	}
	// A bounded page prevents restart recovery from loading every historical build at once.
	if opts.BatchSize <= 0 || opts.BatchSize > models.SDKPackageLeaseBatchLimit {
		opts.BatchSize = defaultSDKGenerationFinalizerBatch
	}
	return opts
}

// Start launches an immediate recovery pass followed by periodic polling.
func (w *SDKGenerationFinalizer) Start(ctx context.Context) {
	// A partially constructed optional worker must remain inert rather than panic at startup.
	if w == nil || w.store == nil || w.client == nil {
		return
	}
	w.startOnce.Do(func() {
		workerCtx, cancel := context.WithCancel(ctx)
		w.cancel = cancel
		go w.run(workerCtx)
	})
}

// Stop cancels polling and waits for the current bounded replay to finish.
func (w *SDKGenerationFinalizer) Stop(ctx context.Context) {
	// An unstarted worker has no goroutine to drain.
	if w == nil || w.cancel == nil {
		return
	}
	w.stopOnce.Do(w.cancel)
	select {
	case <-w.done:
	case <-ctx.Done():
	}
}

// run owns the startup-first schedule so pending rows are recovered before the
// first periodic interval elapses.
func (w *SDKGenerationFinalizer) run(ctx context.Context) {
	defer close(w.done)
	w.finalize(ctx, "startup")
	ticker := time.NewTicker(w.opts.Interval)
	defer ticker.Stop()
	for {
		// Cancellation owns shutdown; the ticker owns every later recovery pass.
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.finalize(ctx, "periodic")
		}
	}
}

// finalize drains one keyset pass and records only bounded state counts.
func (w *SDKGenerationFinalizer) finalize(ctx context.Context, trigger string) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.sdk_generation.finalize")
	defer span.End()
	span.SetAttributes(attribute.String("actor.type", "engine"), attribute.String("generation.trigger", trigger))
	counts, err := w.finalizePages(ctx)
	span.SetAttributes(
		attribute.Int("generation.polled", counts.polled),
		attribute.Int("generation.pending", counts.pending),
		attribute.Int("generation.completed", counts.completed),
		attribute.Int("generation.failed", counts.failed),
		attribute.Int("generation.errors", counts.errors),
	)
	// A failed pass is retried on the next interval and never mutates pending jobs by inference.
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "SDK generation finalization incomplete")
		slog.WarnContext(ctx, "SDK generation finalization incomplete", slog.String("trigger", trigger), slog.Int("error_count", counts.errors))
		return
	}
	span.SetStatus(codes.Ok, "SDK generation finalization complete")
}

type sdkGenerationFinalizerCounts struct {
	polled    int
	pending   int
	completed int
	failed    int
	errors    int
}

// finalizePages scans every currently pending build once using stable app-ID
// keyset pagination, allowing concurrent CAS winners without offset drift.
func (w *SDKGenerationFinalizer) finalizePages(ctx context.Context) (sdkGenerationFinalizerCounts, error) {
	var counts sdkGenerationFinalizerCounts
	after := uuid.Nil
	for {
		builds, err := w.store.ListPendingSDKGenerationBuilds(ctx, after, w.opts.BatchSize)
		// Database ambiguity ends this pass without guessing which rows were observed.
		if err != nil {
			counts.errors++
			return counts, fmt.Errorf("list pending SDK generation builds: %w", err)
		}
		// An empty page proves this startup or periodic pass is complete.
		if len(builds) == 0 {
			return counts, finalizerError(counts.errors)
		}
		w.finalizePage(ctx, builds, &counts)
		after = builds[len(builds)-1].Request.AppID
		// A short page proves there are no later app IDs in this pass.
		if len(builds) < w.opts.BatchSize {
			return counts, finalizerError(counts.errors)
		}
	}
}

// finalizePage isolates one Registry or validation failure so unrelated SDK
// builds in the same bounded page can still progress.
func (w *SDKGenerationFinalizer) finalizePage(ctx context.Context, builds []store.SDKGenerationBuild, counts *sdkGenerationFinalizerCounts) {
	for i := range builds {
		counts.polled++
		outcome, err := w.finalizeBuild(ctx, &builds[i])
		// One malformed or unavailable job remains pending for a later safe replay.
		if err != nil {
			counts.errors++
			slog.WarnContext(ctx, "SDK generation build could not be finalized", slog.String("app_id", builds[i].Request.AppID.String()), slog.String("error_code", "sdk_generation_finalization_failed"))
			continue
		}
		counts.add(outcome)
	}
}

// add records one closed worker outcome without using dynamic labels.
func (c *sdkGenerationFinalizerCounts) add(outcome string) {
	// Only the three closed Registry states contribute to bounded telemetry.
	switch outcome {
	case models.SDKGenerationStatusPending:
		c.pending++
	case models.SDKGenerationStatusComplete:
		c.completed++
	case models.SDKGenerationStatusFailed:
		c.failed++
	}
}

// finalizeBuild replays and validates one exact job before applying its
// terminal compare-and-swap transition.
func (w *SDKGenerationFinalizer) finalizeBuild(ctx context.Context, build *store.SDKGenerationBuild) (string, error) {
	requestCtx, cancel := context.WithTimeout(ctx, w.opts.RequestTimeout)
	defer cancel()
	result, err := w.client.GenerateSDK(requestCtx, build.Request)
	// Transport ambiguity must preserve pending state for idempotent recovery.
	if err != nil {
		return "", err
	}
	// Registry output is never trusted merely because the HTTP request succeeded.
	if err := validateSDKGenerationFinalizerResult(build, result); err != nil {
		return "", err
	}
	// Pending work is observable progress, not a lifecycle transition.
	if result.Status == models.SDKGenerationStatusPending {
		return result.Status, nil
	}
	return w.finalizeTerminalBuild(ctx, build, result)
}

// finalizeTerminalBuild applies the job-and-attempt CAS and emits activation only for its exact winner.
func (w *SDKGenerationFinalizer) finalizeTerminalBuild(ctx context.Context, build *store.SDKGenerationBuild, result models.SDKGenerationResult) (string, error) {
	// A confirmed failed job remains non-runnable and stops future polling after the CAS.
	if result.Status == models.SDKGenerationStatusFailed {
		changed, err := w.store.FailSDKGeneration(ctx, result.AppID, result.JobID, build.Request.IdempotencyKey)
		// A lost CAS means another owner already established authoritative state; publish no stale outcome.
		if err != nil || !changed {
			return "", err
		}
		return result.Status, nil
	}
	changed, err := w.store.CompleteSDKGeneration(ctx, result.AppID, result.JobID, build.Request.IdempotencyKey)
	// Cache invalidation belongs only to the worker that made the version runnable.
	if err == nil && changed && w.opts.OnActivated != nil {
		w.opts.OnActivated(ctx, result.AppID)
	}
	// A lost CAS emits neither activation nor a stale terminal worker outcome.
	if err != nil || !changed {
		return "", err
	}
	return result.Status, nil
}

// validateSDKGenerationFinalizerResult binds one Registry response to the
// retained immutable request and concrete scope persisted during apply.
func validateSDKGenerationFinalizerResult(build *store.SDKGenerationBuild, result models.SDKGenerationResult) error {
	// Missing retained identity means the database row cannot authorize any replay result.
	if !validRetainedSDKGenerationIdentity(build) {
		return errors.New("SDK generation build identity is invalid")
	}
	// App, family, account, and job must stay exact across idempotent replay.
	if !sameSDKGenerationResultIdentity(build, result) {
		return errors.New("SDK generation result identity does not match the retained build")
	}
	// Failed is the only terminal result that legitimately has no generated scope to validate.
	if result.Status == models.SDKGenerationStatusFailed {
		return nil
	}
	// Only pending and complete are valid Registry states for a package-backed build.
	if result.Status != models.SDKGenerationStatusPending && result.Status != models.SDKGenerationStatusComplete {
		return errors.New("SDK generation result status is invalid")
	}
	// Schema and generator fences prevent activation under a different runtime contract.
	if !sameSDKGenerationResultContract(build, result) {
		return errors.New("SDK generation result contract does not match the retained build")
	}
	// The initial apply already admitted this concrete scope; later replay may not replace it.
	if !reflect.DeepEqual(result.Selections, models.SDKSelections(build.Request.Selections)) {
		return errors.New("SDK generation result selections do not match the retained build")
	}
	return nil
}

// validRetainedSDKGenerationIdentity checks the local row is complete enough to authorize Registry replay.
func validRetainedSDKGenerationIdentity(build *store.SDKGenerationBuild) bool {
	return build != nil && build.AccountID != uuid.Nil && build.Request.AppID != uuid.Nil &&
		build.Request.AppFamilyID != uuid.Nil && strings.TrimSpace(build.JobID) != "" &&
		build.Status == models.SDKGenerationStatusPending
}

// sameSDKGenerationResultIdentity compares every durable tenancy and job dimension before interpreting status.
func sameSDKGenerationResultIdentity(build *store.SDKGenerationBuild, result models.SDKGenerationResult) bool {
	return result.AppID == build.Request.AppID && result.AppFamilyID == build.Request.AppFamilyID &&
		result.AccountID == build.AccountID && result.JobID == build.JobID
}

// sameSDKGenerationResultContract prevents Registry replay from changing schema or generator authority.
func sameSDKGenerationResultContract(build *store.SDKGenerationBuild, result models.SDKGenerationResult) bool {
	return result.ScopeSchemaVersion == models.AppScopeSchemaVersion && result.GeneratorVersion == build.Request.GeneratorVersion
}

// finalizerError summarizes per-build failures without retaining Registry or
// generated-content error prose.
func finalizerError(count int) error {
	// A zero error count keeps the successful pass allocation-free.
	if count == 0 {
		return nil
	}
	return fmt.Errorf("%d SDK generation builds remain unresolved", count)
}
