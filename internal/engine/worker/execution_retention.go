package worker

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const executionRetentionInterval = 24 * time.Hour

type executionRetentionStore interface {
	DeleteEngineExecutionEventsBefore(context.Context, time.Time, int) (int64, error)
}

type ExecutionRetentionWorker struct {
	lifecycle *workerLifecycle
}

// StartDynamicExecutionRetentionWorker reads the retention period before each
// cleanup pass so a heartbeat plan change takes effect without restarting the
// Engine. A non-positive value skips that pass without stopping the worker.
func StartDynamicExecutionRetentionWorker(ctx context.Context, store executionRetentionStore, retentionDays func() int, batchSize int) *ExecutionRetentionWorker {
	if store == nil || retentionDays == nil || batchSize <= 0 {
		return nil
	}
	workerCtx, lifecycle := newStartedLifecycle(ctx)
	worker := &ExecutionRetentionWorker{lifecycle: lifecycle}
	go worker.run(workerCtx, store, retentionDays, batchSize)
	return worker
}

// Stop cancels periodic retention sweeps and drains any active cleanup pass.
func (w *ExecutionRetentionWorker) Stop(ctx context.Context) {
	// Startup may skip retention cleanup when disabled or misconfigured.
	if w == nil {
		return
	}
	w.lifecycle.stop(ctx)
}

func (w *ExecutionRetentionWorker) run(ctx context.Context, store executionRetentionStore, retentionDays func() int, batchSize int) {
	defer close(w.lifecycle.done)
	cleanupExecutionRetentionPass(ctx, store, time.Now().UTC(), retentionDays(), batchSize)
	ticker := time.NewTicker(executionRetentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			cleanupExecutionRetentionPass(ctx, store, now.UTC(), retentionDays(), batchSize)
		}
	}
}

// cleanupExecutionRetentionPass always expires private bodies, even when ordinary receipt retention is disabled.
func cleanupExecutionRetentionPass(ctx context.Context, store executionRetentionStore, now time.Time, retentionDays, batchSize int) {
	cutoff := time.Time{}
	// Private bodies honor a shorter configured receipt history without extending their own seven-day maximum.
	if retentionDays > 0 {
		cutoff = now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	}
	cleanupWebhookDiagnostics(ctx, store, cutoff, batchSize)
	// Disabled receipt retention does not disable the private-evidence expiry above.
	if retentionDays <= 0 {
		return
	}
	cleanupExpiredWebhookConsumptions(ctx, store, cutoff, batchSize)
	cleanupExpiredExecutionEvents(ctx, store, cutoff, batchSize)
}

// cleanupExpiredWebhookConsumptions expires per-consumer delivery rows on the same schedule as execution receipts.
func cleanupExpiredWebhookConsumptions(ctx context.Context, repository executionRetentionStore, before time.Time, batchSize int) {
	cleaner, ok := repository.(interface {
		DeleteExpiredWebhookConsumptions(context.Context, time.Time, int) (int64, error)
	})
	// Older test stores without consumption persistence need no retention pass.
	if !ok {
		return
	}
	drainRetentionBatches(
		ctx,
		"engine.webhook.consumptions.retention",
		"consumptions.deleted_count",
		batchSize,
		func(batchCtx context.Context) (int64, error) {
			return cleaner.DeleteExpiredWebhookConsumptions(batchCtx, before, batchSize)
		},
		func(batchCtx context.Context, err error) bool {
			slog.ErrorContext(batchCtx, "Failed to delete expired webhook consumptions", slog.Any("error", err))
			return true
		},
	)
}

// cleanupWebhookDiagnostics drains bounded encrypted-body batches without recording payloads or raw storage errors.
func cleanupWebhookDiagnostics(ctx context.Context, repository executionRetentionStore, cutoff time.Time, batchSize int) {
	cleaner, ok := repository.(interface {
		DeleteExpiredWebhookDiagnostics(context.Context, time.Time, int) (int64, error)
	})
	// Older stores have no private body table and need no alternate retention path.
	if !ok {
		return
	}
	drainRetentionBatches(
		ctx,
		"engine.webhook.diagnostics.retention",
		"diagnostics.deleted_count",
		batchSize,
		func(batchCtx context.Context) (int64, error) {
			return cleaner.DeleteExpiredWebhookDiagnostics(batchCtx, cutoff, batchSize)
		},
		// Never echo raw storage errors for private encrypted bodies into telemetry or logs.
		func(context.Context, error) bool { return false },
	)
}

// drainRetentionBatches repeatedly deletes bounded slices under one span until
// a partial batch confirms all expired rows for this pass have been removed.
func drainRetentionBatches(
	ctx context.Context,
	spanName, deletedCountAttr string,
	batchSize int,
	deleteBatch func(context.Context) (int64, error),
	onError func(context.Context, error) (recordSpanError bool),
) {
	ctx, span := otel.Tracer("engine").Start(ctx, spanName)
	defer span.End()
	var total int64
	for ctx.Err() == nil {
		deleted, err := deleteBatch(ctx)
		// Stop this retention pass on storage failure so the next scheduled cycle can retry cleanly.
		if err != nil {
			// Private diagnostic tables suppress raw errors while standard tables record them on the span.
			if onError(ctx, err) {
				span.RecordError(err)
			} else {
				span.SetAttributes(attribute.Bool("cleanup.failed", true))
			}
			return
		}
		total += deleted
		// A partial batch proves no additional expired rows remain for this cutoff.
		if deleted < int64(batchSize) {
			span.SetAttributes(attribute.Int64(deletedCountAttr, total))
			return
		}
	}
}

// cleanupExpiredExecutionEvents drains expired execution telemetry rows in bounded batches.
func cleanupExpiredExecutionEvents(ctx context.Context, store executionRetentionStore, before time.Time, batchSize int) {
	drainRetentionBatches(
		ctx,
		"engine.execution_events.retention",
		"execution.deleted_count",
		batchSize,
		func(ctx context.Context) (int64, error) {
			return store.DeleteEngineExecutionEventsBefore(ctx, before, batchSize)
		},
		func(ctx context.Context, err error) bool {
			slog.ErrorContext(ctx, "Failed to delete expired execution events", slog.Any("error", err))
			return true
		},
	)
}
