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
	executionAppCleanupInterval   = 5 * time.Minute
	executionAppCleanupTimeout    = 5 * time.Second
	executionAppCleanupBatch      = 500
	executionAppCleanupMaxBatches = 4
)

type executionAppCleanupStore interface {
	RecoverStaleExecutionResults(context.Context, time.Time, int) (int64, error)
	DeleteExpiredReplayEvidence(context.Context, time.Time, int) (int64, error)
	DeleteExpiredExecutionResults(context.Context, time.Time, int) (int64, error)
}

// ExecutionAppCleanupWorker finalizes abandoned work and removes expired result and replay rows.
type ExecutionAppCleanupWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// StartExecutionAppCleanupWorker starts bounded cleanup against Engine-owned result storage.
func StartExecutionAppCleanupWorker(ctx context.Context, repository executionAppCleanupStore) *ExecutionAppCleanupWorker {
	// A missing optional store disables cleanup without starting an idle goroutine.
	if repository == nil {
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &ExecutionAppCleanupWorker{cancel: cancel, done: make(chan struct{})}
	go worker.run(workerCtx, repository)
	return worker
}

// Stop waits for the bounded active cleanup pass or its caller's shutdown deadline.
func (worker *ExecutionAppCleanupWorker) Stop(ctx context.Context) {
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
func (worker *ExecutionAppCleanupWorker) run(ctx context.Context, repository executionAppCleanupStore) {
	defer close(worker.done)
	cleanupExecutionAppPass(ctx, repository, time.Now().UTC())
	ticker := time.NewTicker(executionAppCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			cleanupExecutionAppPass(ctx, repository, now.UTC())
		}
	}
}

// cleanupExecutionAppPass recovers and expires fixed row batches within one short deadline.
func cleanupExecutionAppPass(parent context.Context, repository executionAppCleanupStore, now time.Time) {
	// Shutdown does not start a fresh pass or report cancellation as a database failure.
	if parent.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, executionAppCleanupTimeout)
	defer cancel()
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_app.cleanup")
	defer span.End()
	// The worker budget exceeds the sandbox wall bound, so only abandoned calls are finalized.
	recovered, err := cleanupExecutionAppBatches(ctx, now.Add(-2*time.Minute), repository.RecoverStaleExecutionResults)
	if err != nil {
		// Cancellation is normal shutdown; other failures carry only a bounded diagnostic code.
		if ctx.Err() != nil {
			return
		}
		span.SetStatus(codes.Error, "execution_recovery_failed")
		slog.ErrorContext(ctx, "Failed to recover abandoned capability executions", slog.String("error_code", "execution_recovery_failed"))
		return
	}
	evidenceDeleted, err := cleanupExecutionAppBatches(ctx, now, repository.DeleteExpiredReplayEvidence)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		span.SetStatus(codes.Error, "replay_cleanup_failed")
		slog.ErrorContext(ctx, "Failed to delete expired capability replay evidence", slog.String("error_code", "replay_cleanup_failed"))
		return
	}
	resultsDeleted, err := cleanupExecutionAppBatches(ctx, now, repository.DeleteExpiredExecutionResults)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		span.SetStatus(codes.Error, "result_cleanup_failed")
		slog.ErrorContext(ctx, "Failed to delete expired capability results", slog.String("error_code", "result_cleanup_failed"))
		return
	}
	span.SetAttributes(attribute.Int64("execution_app.results_recovered", recovered), attribute.Int64("execution_app.evidence_deleted", evidenceDeleted), attribute.Int64("execution_app.results_deleted", resultsDeleted))
}

// cleanupExecutionAppBatches caps SQL work per pass even when an Engine has a large backlog.
func cleanupExecutionAppBatches(ctx context.Context, before time.Time, deleteBatch func(context.Context, time.Time, int) (int64, error)) (int64, error) {
	var total int64
	for batch := 0; batch < executionAppCleanupMaxBatches; batch++ {
		// A canceled worker leaves remaining rows for its next replica or pass.
		if err := ctx.Err(); err != nil {
			return total, err
		}
		deleted, err := deleteBatch(ctx, before, executionAppCleanupBatch)
		if err != nil {
			return total, err
		}
		total += deleted
		if deleted < executionAppCleanupBatch {
			return total, nil
		}
	}
	return total, nil
}
