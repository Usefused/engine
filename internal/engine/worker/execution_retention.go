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
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &ExecutionRetentionWorker{cancel: cancel, done: make(chan struct{})}
	go worker.run(workerCtx, store, time.Duration(retentionDays)*24*time.Hour, batchSize)
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

func (w *ExecutionRetentionWorker) run(ctx context.Context, store executionRetentionStore, retention time.Duration, batchSize int) {
	defer close(w.done)
	cleanupExpiredExecutionEvents(ctx, store, time.Now().UTC().Add(-retention), batchSize)
	ticker := time.NewTicker(executionRetentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			cleanupExpiredExecutionEvents(ctx, store, now.UTC().Add(-retention), batchSize)
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
