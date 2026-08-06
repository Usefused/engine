package worker

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultSDKPackageLeaseInterval = 6 * time.Hour
	defaultSDKPackageLeaseJitter   = 15 * time.Minute
	defaultSDKPackageLeaseTimeout  = 15 * time.Second
)

type SDKPackageLeaseStore interface {
	ListSDKPackageLeaseRenewals(context.Context, uuid.UUID, int) ([]models.SDKPackageLeaseRenewal, error)
}

type SDKPackageLeaseClient interface {
	RenewSDKPackageLeases(context.Context, []models.SDKPackageLeaseRenewal) (int64, error)
}

type SDKPackageLeaseOptions struct {
	Interval       time.Duration
	MaxJitter      time.Duration
	BatchSize      int
	RequestTimeout time.Duration
}

type SDKPackageLeaseWorker struct {
	store     SDKPackageLeaseStore
	client    SDKPackageLeaseClient
	opts      SDKPackageLeaseOptions
	resumeAt  uuid.UUID
	cancel    context.CancelFunc
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewSDKPackageLeaseWorker(store SDKPackageLeaseStore, client SDKPackageLeaseClient, opts SDKPackageLeaseOptions) *SDKPackageLeaseWorker {
	return &SDKPackageLeaseWorker{
		store: store, client: client, opts: normalizeSDKPackageLeaseOptions(opts), done: make(chan struct{}),
	}
}

func normalizeSDKPackageLeaseOptions(opts SDKPackageLeaseOptions) SDKPackageLeaseOptions {
	if opts.Interval <= 0 {
		opts.Interval = defaultSDKPackageLeaseInterval
	}
	if opts.MaxJitter < 0 {
		opts.MaxJitter = 0
	} else if opts.MaxJitter == 0 && opts.Interval == defaultSDKPackageLeaseInterval {
		opts.MaxJitter = defaultSDKPackageLeaseJitter
	}
	if opts.BatchSize <= 0 || opts.BatchSize > models.SDKPackageLeaseBatchLimit {
		opts.BatchSize = models.SDKPackageLeaseBatchLimit
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultSDKPackageLeaseTimeout
	}
	return opts
}

func (w *SDKPackageLeaseWorker) Start(ctx context.Context) {
	if w == nil || w.store == nil || w.client == nil {
		return
	}
	w.startOnce.Do(func() {
		workerCtx, cancel := context.WithCancel(ctx)
		w.cancel = cancel
		go w.run(workerCtx)
	})
}

func (w *SDKPackageLeaseWorker) Stop(ctx context.Context) {
	if w == nil || w.cancel == nil {
		return
	}
	w.stopOnce.Do(w.cancel)
	select {
	case <-w.done:
	case <-ctx.Done():
	}
}

func (w *SDKPackageLeaseWorker) run(ctx context.Context) {
	defer close(w.done)
	w.renew(ctx, "startup")
	timer := time.NewTimer(w.nextInterval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			w.renew(ctx, "periodic")
			timer.Reset(w.nextInterval())
		}
	}
}

func (w *SDKPackageLeaseWorker) renew(ctx context.Context, trigger string) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.sdk_package_leases.renew")
	defer span.End()
	span.SetAttributes(attribute.String("actor.type", "engine"), attribute.String("renewal.trigger", trigger))

	requested, renewed, batches, err := w.renewPages(ctx)
	span.SetAttributes(
		attribute.Int("sdk_package.requested", requested),
		attribute.Int64("sdk_package.renewed", renewed),
		attribute.Int("sdk_package.batches", batches),
	)
	if err != nil {
		w.recordRenewalError(ctx, span, trigger, err)
		return
	}
	span.SetStatus(codes.Ok, "SDK package leases renewed")
}

func (w *SDKPackageLeaseWorker) renewPages(ctx context.Context) (int, int64, int, error) {
	requested, renewed, batches := 0, int64(0), 0
	for {
		apps, err := w.loadPage(ctx)
		if err != nil {
			return requested, renewed, batches, err
		}
		if len(apps) == 0 {
			w.resumeAt = uuid.Nil
			return requested, renewed, batches, nil
		}
		count, err := w.renewPage(ctx, apps)
		if err != nil {
			return requested, renewed, batches, err
		}
		requested += len(apps)
		renewed += count
		batches++
		w.resumeAt = apps[len(apps)-1].AppID
		if len(apps) < w.opts.BatchSize {
			w.resumeAt = uuid.Nil
			return requested, renewed, batches, nil
		}
	}
}

func (w *SDKPackageLeaseWorker) loadPage(ctx context.Context) ([]models.SDKPackageLeaseRenewal, error) {
	requestCtx, cancel := context.WithTimeout(ctx, w.opts.RequestTimeout)
	defer cancel()
	apps, err := w.store.ListSDKPackageLeaseRenewals(requestCtx, w.resumeAt, w.opts.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("list SDK package leases after %s: %w", w.resumeAt, err)
	}
	return apps, nil
}

func (w *SDKPackageLeaseWorker) renewPage(ctx context.Context, apps []models.SDKPackageLeaseRenewal) (int64, error) {
	requestCtx, cancel := context.WithTimeout(ctx, w.opts.RequestTimeout)
	defer cancel()
	renewed, err := w.client.RenewSDKPackageLeases(requestCtx, apps)
	if err != nil {
		return 0, fmt.Errorf("renew SDK package lease batch after %s: %w", w.resumeAt, err)
	}
	return renewed, nil
}

func (w *SDKPackageLeaseWorker) recordRenewalError(ctx context.Context, span trace.Span, trigger string, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	span.SetAttributes(attribute.String("sdk_package.resume_after", w.resumeAt.String()))
	slog.WarnContext(ctx, "Failed to renew SDK package leases",
		slog.String("trigger", trigger), slog.String("resume_after", w.resumeAt.String()), slog.Any("error", err))
}

func (w *SDKPackageLeaseWorker) nextInterval() time.Duration {
	if w.opts.MaxJitter <= 0 {
		return w.opts.Interval
	}
	offset := time.Duration(rand.Int64N(int64(2*w.opts.MaxJitter)+1)) - w.opts.MaxJitter
	delay := w.opts.Interval + offset
	if delay <= 0 {
		return time.Second
	}
	return delay
}
