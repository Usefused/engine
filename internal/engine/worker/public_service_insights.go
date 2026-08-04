package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/shared/models"
)

const (
	defaultPublicInsightInterval     = time.Minute
	defaultPublicInsightServiceLimit = 500
	defaultPublicInsightEventLimit   = 10000
	defaultPublicInsightReportLimit  = 500
)

type PublicInsightStore interface {
	ListUnprojectedPublicInsightServiceIDs(context.Context, time.Time, int) ([]uuid.UUID, error)
	ProjectPublicServiceInsightReports(context.Context, []uuid.UUID, time.Time, int) (int64, error)
	ListPendingPublicServiceInsightReports(context.Context, int, time.Time) ([]models.PublicServiceInsightReport, error)
	MarkPublicServiceInsightReportResults(context.Context, []models.PublicServiceInsightReportResult, time.Time) error
	MarkPublicServiceInsightReportDeliveryFailure(context.Context, []uuid.UUID, string, time.Time) error
}

type PublicInsightClient interface {
	FetchPublicServiceInsightEligibility(context.Context, []uuid.UUID) (map[uuid.UUID]bool, error)
	SendPublicServiceInsightReports(context.Context, string, string, []models.PublicServiceInsightReport, time.Time) ([]models.PublicServiceInsightReportResult, error)
}

type PublicInsightOptions struct {
	Interval        time.Duration
	ServiceLimit    int
	EventLimit      int
	ReportLimit     int
	EngineVersion   string
	EngineBuildHash string
}

type PublicInsightWorker struct {
	store  PublicInsightStore
	client PublicInsightClient
	opts   PublicInsightOptions
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func NewPublicInsightWorker(store PublicInsightStore, client PublicInsightClient, opts PublicInsightOptions) *PublicInsightWorker {
	if opts.Interval <= 0 {
		opts.Interval = defaultPublicInsightInterval
	}
	if opts.ServiceLimit <= 0 {
		opts.ServiceLimit = defaultPublicInsightServiceLimit
	}
	if opts.EventLimit <= 0 {
		opts.EventLimit = defaultPublicInsightEventLimit
	}
	if opts.ReportLimit <= 0 {
		opts.ReportLimit = defaultPublicInsightReportLimit
	}
	return &PublicInsightWorker{store: store, client: client, opts: opts, done: make(chan struct{})}
}

func (w *PublicInsightWorker) Start(ctx context.Context) {
	workerCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go w.run(workerCtx)
}

func (w *PublicInsightWorker) Stop(ctx context.Context) {
	if w == nil || w.cancel == nil {
		return
	}
	w.once.Do(w.cancel)
	select {
	case <-w.done:
	case <-ctx.Done():
	}
}

func (w *PublicInsightWorker) run(ctx context.Context) {
	defer close(w.done)
	w.flush(ctx)
	ticker := time.NewTicker(w.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.flush(ctx)
		}
	}
}

func (w *PublicInsightWorker) flush(ctx context.Context) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.public_service_insights.flush")
	defer span.End()
	now := time.Now().UTC()
	closedBefore := now.Truncate(time.Hour)
	serviceIDs, err := w.store.ListUnprojectedPublicInsightServiceIDs(ctx, closedBefore, w.opts.ServiceLimit)
	if err != nil {
		w.recordError(ctx, span, "list candidates", err)
		return
	}
	if len(serviceIDs) > 0 {
		if err := w.projectReportableServices(ctx, serviceIDs, closedBefore); err != nil {
			w.recordError(ctx, span, "project reports", err)
			return
		}
	}
	reports, err := w.store.ListPendingPublicServiceInsightReports(ctx, w.opts.ReportLimit, now)
	if err != nil {
		w.recordError(ctx, span, "list reports", err)
		return
	}
	span.SetAttributes(attribute.Int("public_insights.report_count", len(reports)))
	if len(reports) == 0 {
		return
	}
	results, err := w.client.SendPublicServiceInsightReports(ctx, w.opts.EngineVersion, w.opts.EngineBuildHash, reports, now)
	if err != nil {
		_ = w.store.MarkPublicServiceInsightReportDeliveryFailure(ctx, publicInsightReportIDs(reports), "registry_unavailable", now)
		w.recordError(ctx, span, "send reports", err)
		return
	}
	if err := w.store.MarkPublicServiceInsightReportResults(ctx, results, now); err != nil {
		w.recordError(ctx, span, "mark reports", err)
	}
}

func (w *PublicInsightWorker) projectReportableServices(ctx context.Context, serviceIDs []uuid.UUID, before time.Time) error {
	eligibility, err := w.client.FetchPublicServiceInsightEligibility(ctx, serviceIDs)
	if err != nil {
		return err
	}
	reportable := make([]uuid.UUID, 0, len(serviceIDs))
	for _, serviceID := range serviceIDs {
		if eligibility[serviceID] {
			reportable = append(reportable, serviceID)
		}
	}
	_, err = w.store.ProjectPublicServiceInsightReports(ctx, reportable, before, w.opts.EventLimit)
	return err
}

func (w *PublicInsightWorker) recordError(ctx context.Context, span trace.Span, action string, err error) {
	span.RecordError(err)
	slog.WarnContext(ctx, "Public service insight worker failed", slog.String("action", action), slog.Any("error", err))
}

func publicInsightReportIDs(reports []models.PublicServiceInsightReport) []uuid.UUID {
	ids := make([]uuid.UUID, len(reports))
	for index := range reports {
		ids[index] = reports[index].ReportID
	}
	return ids
}
