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
	defaultUsageReportFlushInterval = time.Minute
	defaultUsageReportBatchLimit    = 500
)

type RuntimeUsageReportStore interface {
	ListPendingRuntimeUsageReports(ctx context.Context, limit int) ([]models.EngineUsageReport, error)
	MarkRuntimeUsageReportsFlushed(ctx context.Context, reportIDs []uuid.UUID, flushedAt time.Time) error
}

type RuntimeUsageReportClient interface {
	SendUsageReports(ctx context.Context, engineVersion, engineBuildHash string, reports []models.EngineUsageReport, reportedAt time.Time) error
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
