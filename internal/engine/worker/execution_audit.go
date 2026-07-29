package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
)

const (
	defaultExecutionAuditQueueSize     = 1000
	defaultExecutionAuditBatchSize     = 100
	defaultExecutionAuditFlushInterval = time.Second
)

type executionAuditStore interface {
	BatchCreateEngineExecutionEvents(ctx context.Context, events []models.EngineExecutionEvent) error
}

type ExecutionAuditOptions struct {
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
}

type ExecutionAuditWorker struct {
	store         executionAuditStore
	events        chan models.EngineExecutionEvent
	batchSize     int
	flushInterval time.Duration
	done          chan struct{}
	mu            sync.RWMutex
	started       bool
	stopped       bool
	stopOnce      sync.Once
	startOnce     sync.Once
}

func NewExecutionAuditWorker(store executionAuditStore, opts ExecutionAuditOptions) *ExecutionAuditWorker {
	queueSize := opts.QueueSize
	if queueSize <= 0 {
		queueSize = defaultExecutionAuditQueueSize
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultExecutionAuditBatchSize
	}
	flushInterval := opts.FlushInterval
	if flushInterval <= 0 {
		flushInterval = defaultExecutionAuditFlushInterval
	}
	return &ExecutionAuditWorker{
		store:         store,
		events:        make(chan models.EngineExecutionEvent, queueSize),
		batchSize:     batchSize,
		flushInterval: flushInterval,
		done:          make(chan struct{}),
	}
}

func (w *ExecutionAuditWorker) Start(ctx context.Context) {
	if w == nil || w.store == nil {
		return
	}
	w.startOnce.Do(func() {
		w.mu.Lock()
		w.started = true
		w.mu.Unlock()
		go w.run(ctx)
	})
}

func (w *ExecutionAuditWorker) Stop(ctx context.Context) {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopped = true
		close(w.events)
		started := w.started
		w.mu.Unlock()
		if !started {
			return
		}
		select {
		case <-w.done:
		case <-ctx.Done():
		}
	})
}

func (w *ExecutionAuditWorker) Record(event models.EngineExecutionEvent) {
	if w == nil {
		return
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.stopped {
		return
	}
	select {
	case w.events <- event:
	default:
		// Audit receipts should not add user-visible latency. A full queue means
		// Postgres is behind; report the drop so operators can increase capacity.
		slog.Warn("Dropped engine execution audit event: queue full",
			slog.String("artifact_id", event.ArtifactID.String()),
			slog.String("endpoint_name", event.EndpointName),
		)
	}
}

func (w *ExecutionAuditWorker) run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	batch := make([]models.EngineExecutionEvent, 0, w.batchSize)
	flush := func(flushCtx context.Context) {
		if len(batch) == 0 {
			return
		}
		boundedCtx, cancel := boundedWorkerFlushContext(flushCtx)
		defer cancel()
		if err := w.store.BatchCreateEngineExecutionEvents(boundedCtx, batch); err != nil {
			slog.ErrorContext(ctx, "Failed to batch insert engine execution audit events", slog.Any("error", err))
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := finalWorkerFlushContext()
			flush(flushCtx)
			cancel()
			return
		case event, ok := <-w.events:
			if !ok {
				flushCtx, cancel := finalWorkerFlushContext()
				flush(flushCtx)
				cancel()
				return
			}
			batch = append(batch, event)
			if len(batch) >= w.batchSize {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
		}
	}
}
