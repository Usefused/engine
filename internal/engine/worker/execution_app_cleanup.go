package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const (
	unifiedAppCleanupInterval   = 5 * time.Minute
	unifiedAppCleanupTimeout    = 5 * time.Second
	unifiedAppCleanupBatch      = 500
	unifiedAppCleanupMaxBatches = 4
)

type unifiedAppCleanupStore interface {
	RecoverStaleExecutionResults(context.Context, time.Time, int) (int64, error)
	DeleteExpiredReplayEvidence(context.Context, time.Time, int) (int64, error)
	DeleteExpiredExecutionResults(context.Context, time.Time, int) (int64, error)
}

// UnifiedAppCleanupWorker finalizes abandoned work and removes expired result and replay rows.
type UnifiedAppCleanupWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// StartUnifiedAppCleanupWorker starts bounded cleanup against Engine-owned result storage.
func StartUnifiedAppCleanupWorker(ctx context.Context, repository unifiedAppCleanupStore) *UnifiedAppCleanupWorker {
	// A missing optional store disables cleanup without starting an idle goroutine.
	if repository == nil {
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &UnifiedAppCleanupWorker{cancel: cancel, done: make(chan struct{})}
	go worker.run(workerCtx, repository)
	return worker
}

// Stop waits for the bounded active cleanup pass or its caller's shutdown deadline.
func (worker *UnifiedAppCleanupWorker) Stop(ctx context.Context) {
	if worker == nil {
		return
	}
	worker.once.Do(worker.cancel)
	select {
	case <-worker.done:
	case <-ctx.Done():
	}
}

// run makes one startup pass, then periodically revisits abandoned and expired rows.
func (worker *UnifiedAppCleanupWorker) run(ctx context.Context, repository unifiedAppCleanupStore) {
	defer close(worker.done)
	cleanupUnifiedAppPass(ctx, repository, time.Now().UTC())
	ticker := time.NewTicker(unifiedAppCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			cleanupUnifiedAppPass(ctx, repository, now.UTC())
		}
	}
}

// cleanupUnifiedAppPass recovers and expires fixed row batches within one short deadline.
func cleanupUnifiedAppPass(parent context.Context, repository unifiedAppCleanupStore, now time.Time) {
	// Shutdown does not start a fresh pass or report cancellation as a database failure.
	if parent.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, unifiedAppCleanupTimeout)
	defer cancel()
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.unified_app.cleanup")
	defer span.End()
	// The worker budget exceeds the sandbox wall bound, so only abandoned calls are finalized.
	recovered, err := cleanupUnifiedAppBatches(ctx, now.Add(-2*time.Minute), repository.RecoverStaleExecutionResults)
	if err != nil {
		// Cancellation is normal shutdown; other failures carry only a bounded diagnostic code.
		if ctx.Err() != nil {
			return
		}
		span.SetStatus(codes.Error, "execution_recovery_failed")
		slog.ErrorContext(ctx, "Failed to recover abandoned capability executions", slog.String("error_code", "execution_recovery_failed"))
		return
	}
	evidenceDeleted, err := cleanupUnifiedAppBatches(ctx, now, repository.DeleteExpiredReplayEvidence)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		span.SetStatus(codes.Error, "replay_cleanup_failed")
		slog.ErrorContext(ctx, "Failed to delete expired capability replay evidence", slog.String("error_code", "replay_cleanup_failed"))
		return
	}
	resultsDeleted, err := cleanupUnifiedAppBatches(ctx, now, repository.DeleteExpiredExecutionResults)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		span.SetStatus(codes.Error, "result_cleanup_failed")
		slog.ErrorContext(ctx, "Failed to delete expired capability results", slog.String("error_code", "result_cleanup_failed"))
		return
	}
	span.SetAttributes(attribute.Int64("unified_app.results_recovered", recovered), attribute.Int64("unified_app.evidence_deleted", evidenceDeleted), attribute.Int64("unified_app.results_deleted", resultsDeleted))
}

// cleanupUnifiedAppBatches caps SQL work per pass even when an Engine has a large backlog.
func cleanupUnifiedAppBatches(ctx context.Context, before time.Time, deleteBatch func(context.Context, time.Time, int) (int64, error)) (int64, error) {
	var total int64
	for batch := 0; batch < unifiedAppCleanupMaxBatches; batch++ {
		// A canceled worker leaves remaining rows for its next replica or pass.
		if err := ctx.Err(); err != nil {
			return total, err
		}
		deleted, err := deleteBatch(ctx, before, unifiedAppCleanupBatch)
		if err != nil {
			return total, err
		}
		total += deleted
		if deleted < unifiedAppCleanupBatch {
			return total, nil
		}
	}
	return total, nil
}
