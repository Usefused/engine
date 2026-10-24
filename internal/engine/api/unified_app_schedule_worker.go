package api

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// UnifiedAppScheduleWorker owns bounded discovery, cancellation, and joining of scheduled invocations.
type UnifiedAppScheduleWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// StartUnifiedAppScheduleWorker uses the shared executor; no HTTP token or separate execution runtime is created.
func (s *EngineGRPCServer) StartUnifiedAppScheduleWorker(ctx context.Context) (*UnifiedAppScheduleWorker, error) {
	repository, ok := s.store.(store.UnifiedAppScheduleStore)
	// Scheduling cannot fall back to process-local timers when durable storage is unavailable.
	if !ok {
		return nil, errors.New("Unified App schedule store unavailable")
	}
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &UnifiedAppScheduleWorker{cancel: cancel, done: make(chan struct{})}
	// One bounded loop joins each batch before polling again, including during graceful shutdown.
	go func() {
		defer close(worker.done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			s.runScheduleBatch(workerCtx, repository)
			// Recovery uses the same bounded, cancellable scheduler lifecycle as cron.
			s.runExecutionRecoveryBatch(workerCtx)
			// Cancellation prevents new admissions after the active batch drains.
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return worker, nil
}

// Stop bounds shutdown while cancelling all scheduled calls through their ordinary runtime context.
func (worker *UnifiedAppScheduleWorker) Stop(ctx context.Context) {
	// Startup may omit this worker in partial test wiring.
	if worker == nil {
		return
	}
	worker.cancel()
	select {
	case <-worker.done:
	case <-ctx.Done():
	}
}

// runScheduleBatch pauses uncertain interrupted work before admitting a bounded set of due schedules.
func (s *EngineGRPCServer) runScheduleBatch(ctx context.Context, repository store.UnifiedAppScheduleStore) {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Recovery is safe even while a license blocks fresh execution.
	if err := s.recoverScheduledRuns(readCtx, repository); err != nil {
		slog.WarnContext(ctx, "Scheduled execution recovery unavailable")
		return
	}
	// Background triggers must enforce the same suspension and heartbeat boundary as HTTP execution.
	if ctx.Err() != nil || entitlement.EngineSuspended.Load() || entitlement.EngineHeartbeatLeaseExpired.Load() {
		return
	}
	items, err := repository.DueUnifiedAppSchedules(readCtx, s.masterKey)
	// Never run a partial or stale discovery snapshot on a storage failure.
	if err != nil {
		slog.WarnContext(ctx, "Unified App schedule discovery unavailable")
		return
	}
	var running sync.WaitGroup
	// Discovery is capped at 32; actual execution still uses the shared admission/concurrency limits.
	for _, item := range items {
		running.Add(1)
		// Each occurrence retains an independent deadline but shares Engine shutdown cancellation.
		go func(item store.UnifiedAppSchedule) {
			defer running.Done()
			s.runScheduledApp(ctx, repository, item)
		}(item)
	}
	running.Wait()
}

// scheduledRunSpec resolves current traffic immediately before the database fences its immutable execution identity.
func (s *EngineGRPCServer) scheduledRunSpec(ctx context.Context, item store.UnifiedAppSchedule) (capabilityRunSpec, error) {
	targets, ok := s.store.(store.UnifiedAppPromotionStore)
	// No inferred latest-version selection is allowed for automatic execution.
	if !ok {
		return capabilityRunSpec{}, errors.New("schedule target unavailable")
	}
	id, err := targets.UnifiedAppTrafficTarget(ctx, item.FamilyID)
	if err != nil || id == uuid.Nil {
		return capabilityRunSpec{}, store.ErrAppRuntimeNotFound
	}
	app, err := s.store.GetAppRuntime(ctx, id)
	// Saved schedules cannot cross account, adapter, or family boundaries after lifecycle changes.
	if err != nil {
		return capabilityRunSpec{}, err
	}
	if app.AccountID != item.AccountID || app.AppFamilyID != item.FamilyID || app.Kind != store.AppKindUnifiedApp {
		return capabilityRunSpec{}, store.ErrAppRuntimeNotFound
	}
	bundle, manifest, found, requestErr := s.findUnifiedAppBundle(ctx, id)
	// Bundle integrity and current runtime eligibility share ordinary execution's checks.
	if requestErr != nil || !found {
		return capabilityRunSpec{}, store.ErrUnifiedAppBundleNotFound
	}
	return capabilityRunSpec{identity: auth.RuntimeIdentity{AccountID: item.AccountID, AppFamilyID: item.FamilyID, AppID: id, AppVersion: app.Version, Kind: store.AppKindUnifiedApp},
		version: app.Version, bundle: bundle, manifest: manifest, input: item.Input, mode: "live", transport: "schedule", schedule: &item}, nil
}

// runScheduledApp reserves once and never automatically retries a potentially effectful execution.
func (s *EngineGRPCServer) runScheduledApp(parent context.Context, repository store.UnifiedAppScheduleStore, item store.UnifiedAppSchedule) {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.unified_app.scheduled_run")
	defer span.End()
	span.SetAttributes(attribute.String("app.family_id", item.FamilyID.String()), attribute.String("execution.trigger", "schedule"))
	// A license change during discovery must prevent fresh automatic effects.
	if ctx.Err() != nil || entitlement.EngineSuspended.Load() || entitlement.EngineHeartbeatLeaseExpired.Load() {
		return
	}
	spec, err := s.scheduledRunSpec(ctx, item)
	// Pre-admission failures leave timing intact for a later safe attempt.
	if err != nil {
		span.SetStatus(codes.Error, "schedule_target_unavailable")
		return
	}
	admitted, requestErr := s.admitCapabilityRun(ctx, spec)
	if requestErr != nil {
		span.SetStatus(codes.Error, "schedule_admission_failed")
		return
	}
	// Only the replica that advanced this occurrence may enter the provider-capable worker.
	if !admitted.created {
		return
	}
	span.SetAttributes(attribute.String("app.id", spec.identity.AppID.String()), attribute.String("execution.id", admitted.id.String()))
	result, requestErr := s.runAdmittedCapability(ctx, spec, admitted)
	finishUnifiedAppSpan(span, result, requestErr)
	status := result.Status
	// Unknown persistence outcomes pause recurrence rather than guessing that no side effects occurred.
	if requestErr != nil {
		// Leave the running marker for recovery to close the result and publish its canonical receipt together.
		return
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finishCancel()
	// A failed completion update is recovered conservatively by the abandoned-run sweep.
	if err := repository.FinishScheduledExecution(finishCtx, item.ID, admitted.id, status); err != nil {
		span.SetStatus(codes.Error, "schedule_completion_unavailable")
	}
}

// recoverScheduledRuns publishes interrupted outcomes through the ordinary execution history writer.
func (s *EngineGRPCServer) recoverScheduledRuns(ctx context.Context, repository store.UnifiedAppScheduleStore) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.unified_app.schedule_recovery")
	defer span.End()
	results, err := repository.RecoverScheduledExecutions(ctx)
	span.SetAttributes(attribute.Int("execution.recovered_count", len(results)))
	// Storage owns the terminal transition; a failed sweep must not invent interrupted runs.
	if err != nil {
		return err
	}
	for _, record := range results {
		// Canonical event identity deduplicates these receipts alongside normal execution publication.
		if err := executionevent.Publish(ctx, unifiedAppReceipt(ctx, &record, capabilityRunSpec{transport: "schedule"})); err != nil {
			return err
		}
	}
	return nil
}
