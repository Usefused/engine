package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const (
	defaultUsageCounterQueueSize     = 1000
	defaultUsageCounterBatchSize     = 100
	defaultUsageCounterFlushInterval = time.Second
	defaultUsageReportFlushInterval  = time.Minute
	defaultUsageReportBatchLimit     = 500
)

type RuntimeUsageCounterStore interface {
	IncrementRuntimeUsageCounters(ctx context.Context, increments []models.EngineUsageIncrement) error
}

type RuntimeUsageReportStore interface {
	ListPendingRuntimeUsageReports(ctx context.Context, limit int) ([]models.EngineUsageReport, error)
	MarkRuntimeUsageReportsFlushed(ctx context.Context, reportIDs []uuid.UUID, flushedAt time.Time) error
}

type RuntimeUsageReportClient interface {
	SendUsageReports(ctx context.Context, engineVersion, engineBuildHash string, reports []models.EngineUsageReport, reportedAt time.Time) error
}

type UsageCounterOptions struct {
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
}

type UsageCounterWorker struct {
	store         RuntimeUsageCounterStore
	increments    chan models.EngineUsageIncrement
	batchSize     int
	flushInterval time.Duration
	done          chan struct{}
	mu            sync.RWMutex
	stopped       bool
	started       bool
	startOnce     sync.Once
	stopOnce      sync.Once
}

func NewUsageCounterWorker(store RuntimeUsageCounterStore, opts UsageCounterOptions) *UsageCounterWorker {
	queueSize, batchSize, flushInterval := usageCounterOptions(opts)
	return &UsageCounterWorker{
		store:         store,
		increments:    make(chan models.EngineUsageIncrement, queueSize),
		batchSize:     batchSize,
		flushInterval: flushInterval,
		done:          make(chan struct{}),
	}
}

func (w *UsageCounterWorker) Start(ctx context.Context) {
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

func (w *UsageCounterWorker) Stop(ctx context.Context) {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopped = true
		close(w.increments)
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

func (w *UsageCounterWorker) Record(increment models.EngineUsageIncrement) {
	if w == nil {
		return
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.stopped {
		return
	}
	select {
	case w.increments <- increment:
	default:
		// Usage accounting should never add user-visible execution latency.
		// Dropping is noisy so operators can tune queue/batch settings.
		slog.Warn("Dropped engine usage increment: queue full", slog.String("metric", increment.Metric))
	}
}

func (w *UsageCounterWorker) run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	batch := make([]models.EngineUsageIncrement, 0, w.batchSize)
	flush := func(flushCtx context.Context) {
		if len(batch) == 0 {
			return
		}
		boundedCtx, cancel := boundedWorkerFlushContext(flushCtx)
		defer cancel()
		if err := w.store.IncrementRuntimeUsageCounters(boundedCtx, batch); err != nil {
			slog.ErrorContext(ctx, "Failed to increment engine usage counters", slog.Any("error", err))
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
		case increment, ok := <-w.increments:
			if !ok {
				flushCtx, cancel := finalWorkerFlushContext()
				flush(flushCtx)
				cancel()
				return
			}
			batch = append(batch, increment)
			if len(batch) >= w.batchSize {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
		}
	}
}

func usageCounterOptions(opts UsageCounterOptions) (int, int, time.Duration) {
	queueSize := opts.QueueSize
	if queueSize <= 0 {
		queueSize = defaultUsageCounterQueueSize
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultUsageCounterBatchSize
	}
	flushInterval := opts.FlushInterval
	if flushInterval <= 0 {
		flushInterval = defaultUsageCounterFlushInterval
	}
	return queueSize, batchSize, flushInterval
}

type UsageReportFlushOptions struct {
	Interval        time.Duration
	BatchLimit      int
	EngineVersion   string
	EngineBuildHash string
}

type UsageReportFlushWorker struct {
	store     RuntimeUsageReportStore
	client    RuntimeUsageReportClient
	opts      UsageReportFlushOptions
	stop      chan struct{}
	done      chan struct{}
	mu        sync.RWMutex
	started   bool
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewUsageReportFlushWorker(store RuntimeUsageReportStore, client RuntimeUsageReportClient, opts UsageReportFlushOptions) *UsageReportFlushWorker {
	if opts.Interval <= 0 {
		opts.Interval = defaultUsageReportFlushInterval
	}
	if opts.BatchLimit <= 0 {
		opts.BatchLimit = defaultUsageReportBatchLimit
	}
	return &UsageReportFlushWorker{store: store, client: client, opts: opts, stop: make(chan struct{}), done: make(chan struct{})}
}

func (w *UsageReportFlushWorker) Start(ctx context.Context) {
	if w == nil || w.store == nil || w.client == nil {
		return
	}
	w.startOnce.Do(func() {
		w.mu.Lock()
		w.started = true
		w.mu.Unlock()
		go w.run(ctx)
	})
}

func (w *UsageReportFlushWorker) Stop(ctx context.Context) {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		close(w.stop)
		w.mu.RLock()
		started := w.started
		w.mu.RUnlock()
		if !started {
			return
		}
		select {
		case <-w.done:
		case <-ctx.Done():
		}
	})
}

func (w *UsageReportFlushWorker) run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.opts.Interval)
	defer ticker.Stop()
	w.flush(ctx)
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := finalWorkerFlushContext()
			w.flush(flushCtx)
			cancel()
			return
		case <-w.stop:
			flushCtx, cancel := finalWorkerFlushContext()
			w.flush(flushCtx)
			cancel()
			return
		case <-ticker.C:
			w.flush(ctx)
		}
	}
}

func (w *UsageReportFlushWorker) flush(ctx context.Context) {
	ctx, cancel := boundedWorkerFlushContext(ctx)
	defer cancel()
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.usage.flush")
	defer span.End()

	reports, err := w.store.ListPendingRuntimeUsageReports(ctx, w.opts.BatchLimit)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "Failed to list pending usage reports", slog.Any("error", err))
		return
	}
	if len(reports) == 0 {
		return
	}
	reportedAt := time.Now().UTC()
	span.SetAttributes(attribute.Int("usage_report.count", len(reports)))
	if err := w.client.SendUsageReports(ctx, w.opts.EngineVersion, w.opts.EngineBuildHash, reports, reportedAt); err != nil {
		span.SetStatus(codes.Error, err.Error())
		slog.WarnContext(ctx, "Failed to flush engine usage reports", slog.Any("error", err), slog.Int("count", len(reports)))
		return
	}
	if err := w.store.MarkRuntimeUsageReportsFlushed(ctx, usageReportIDs(reports), reportedAt); err != nil {
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "Failed to mark usage reports flushed", slog.Any("error", err))
		return
	}
	span.SetStatus(codes.Ok, "usage reports flushed")
}

func usageReportIDs(reports []models.EngineUsageReport) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(reports))
	for _, report := range reports {
		ids = append(ids, report.ReportID)
	}
	return ids
}
