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

func cleanupExecutionRetentionPass(ctx context.Context, store executionRetentionStore, now time.Time, retentionDays, batchSize int) {
	if retentionDays <= 0 {
		return
	}
	cleanupExpiredExecutionEvents(ctx, store, now.Add(-time.Duration(retentionDays)*24*time.Hour), batchSize)
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
