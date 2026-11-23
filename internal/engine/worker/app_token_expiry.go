package worker

import (
	"context"
	"log/slog"
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
	lifecycle *workerLifecycle
}

// StartAppTokenExpiryWorker starts periodic expiry sweeps for app execution tokens.
func StartAppTokenExpiryWorker(ctx context.Context, store appTokenExpiryStore, batchSize int) *AppTokenExpiryWorker {
	// Skip spawning a background loop when storage or batch size is invalid.
	if store == nil || batchSize <= 0 {
		return nil
	}
	workerCtx, lifecycle := newStartedLifecycle(ctx)
	worker := &AppTokenExpiryWorker{lifecycle: lifecycle}
	go worker.run(workerCtx, store, batchSize)
	return worker
}

// Stop cancels periodic token expiration and waits for the active pass to finish.
func (worker *AppTokenExpiryWorker) Stop(ctx context.Context) {
	// Nil workers are valid when background expiration is disabled.
	if worker == nil {
		return
	}
	worker.lifecycle.stop(ctx)
}

func (worker *AppTokenExpiryWorker) run(ctx context.Context, store appTokenExpiryStore, batchSize int) {
	defer close(worker.lifecycle.done)
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
