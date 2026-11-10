package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const executionRetentionInterval = 24 * time.Hour

type executionRetentionStore interface {
	DeleteEngineExecutionEventsBefore(context.Context, time.Time, int) (int64, error)
}

type ExecutionRetentionWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func StartExecutionRetentionWorker(ctx context.Context, store executionRetentionStore, retentionDays, batchSize int) *ExecutionRetentionWorker {
	if store == nil || retentionDays <= 0 || batchSize <= 0 {
		return nil
	}
	return StartDynamicExecutionRetentionWorker(ctx, store, func() int { return retentionDays }, batchSize)
}

// StartDynamicExecutionRetentionWorker reads the retention period before each
// cleanup pass so a heartbeat plan change takes effect without restarting the
// Engine. A non-positive value skips that pass without stopping the worker.
func StartDynamicExecutionRetentionWorker(ctx context.Context, store executionRetentionStore, retentionDays func() int, batchSize int) *ExecutionRetentionWorker {
	if store == nil || retentionDays == nil || batchSize <= 0 {
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &ExecutionRetentionWorker{cancel: cancel, done: make(chan struct{})}
	go worker.run(workerCtx, store, retentionDays, batchSize)
	return worker
}

func (w *ExecutionRetentionWorker) Stop(ctx context.Context) {
	if w == nil {
		return
	}
	w.once.Do(w.cancel)
	select {
	case <-w.done:
	case <-ctx.Done():
	}
}

func (w *ExecutionRetentionWorker) run(ctx context.Context, store executionRetentionStore, retentionDays func() int, batchSize int) {
	defer close(w.done)
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
	cleanupExpiredExecutionEvents(ctx, store, now.Add(-time.Duration(retentionDays)*24*time.Hour), batchSize)
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
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.webhook.diagnostics.retention")
	defer span.End()
	var total int64
	for ctx.Err() == nil {
		deleted, err := cleaner.DeleteExpiredWebhookDiagnostics(ctx, cutoff, batchSize)
		// A failure must never echo encrypted or decrypted storage values into telemetry.
		if err != nil {
			span.SetAttributes(attribute.Bool("cleanup.failed", true))
			return
		}
		total += deleted
		// A partial batch proves this pass drained every eligible private record.
		if deleted < int64(batchSize) {
			span.SetAttributes(attribute.Int64("diagnostics.deleted_count", total))
			return
		}
	}
}

func cleanupExpiredExecutionEvents(ctx context.Context, store executionRetentionStore, before time.Time, batchSize int) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_events.retention")
	defer span.End()
	var total int64
	for ctx.Err() == nil {
		deleted, err := store.DeleteEngineExecutionEventsBefore(ctx, before, batchSize)
		if err != nil {
			span.RecordError(err)
			slog.ErrorContext(ctx, "Failed to delete expired execution events", slog.Any("error", err))
			return
		}
		total += deleted
		if deleted < int64(batchSize) {
			span.SetAttributes(attribute.Int64("execution.deleted_count", total))
			return
		}
	}
}
