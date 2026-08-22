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

const appTokenExpiryInterval = time.Minute

type appTokenExpiryStore interface {
	ExpireAppTokens(context.Context, int) (int, error)
}

type AppTokenExpiryWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func StartAppTokenExpiryWorker(ctx context.Context, store appTokenExpiryStore, batchSize int) *AppTokenExpiryWorker {
	if store == nil || batchSize <= 0 {
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &AppTokenExpiryWorker{cancel: cancel, done: make(chan struct{})}
	go worker.run(workerCtx, store, batchSize)
	return worker
}

func (worker *AppTokenExpiryWorker) Stop(ctx context.Context) {
	if worker == nil {
		return
	}
	worker.once.Do(worker.cancel)
	select {
	case <-worker.done:
	case <-ctx.Done():
	}
}

func (worker *AppTokenExpiryWorker) run(ctx context.Context, store appTokenExpiryStore, batchSize int) {
	defer close(worker.done)
	expireAppTokensPass(ctx, store, batchSize)
	ticker := time.NewTicker(appTokenExpiryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			expireAppTokensPass(ctx, store, batchSize)
		}
	}
}

func expireAppTokensPass(ctx context.Context, store appTokenExpiryStore, batchSize int) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.app_tokens.expiry")
	defer span.End()
	total := 0
	for ctx.Err() == nil {
		expired, err := store.ExpireAppTokens(ctx, batchSize)
		if err != nil {
			// Logs intentionally carry only a stable classification; SQL details
			// remain on the recorded error for restricted telemetry backends.
			span.RecordError(err)
			span.SetStatus(codes.Error, "app-token expiry failed")
			slog.ErrorContext(ctx, "Failed to expire app tokens", slog.String("error_code", "app_token_expiry_failed"))
			return
		}
		total += expired
		if expired < batchSize {
			span.SetAttributes(attribute.Int("app.token.expired_count", total))
			return
		}
	}
}
