package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const (
	providerRateLimitConsumer  = "engine_provider_rate_limit_postgres"
	providerRateLimitBatchSize = 100
)

type providerRateLimitProjectionStore interface {
	BatchUpsertProviderRateLimitStates(context.Context, []ratelimitpolicy.StateEnvelope) error
}

type ProviderRateLimitProjectionWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func StartProviderRateLimitProjectionWorker(ctx context.Context, projectionStore providerRateLimitProjectionStore, natsClient *messaging.NATSClient) (*ProviderRateLimitProjectionWorker, error) {
	if projectionStore == nil || natsClient == nil || natsClient.JS == nil {
		return nil, errors.New("provider rate-limit projection requires PostgreSQL and JetStream")
	}
	if err := ensureProviderRateLimitConsumer(natsClient); err != nil {
		return nil, err
	}
	subscription, err := natsClient.JS.PullSubscribe(
		messaging.ProviderRateLimitKVSubject(), providerRateLimitConsumer,
		nats.BindStream(messaging.ProviderRateLimitKVStream()),
	)
	if err != nil {
		return nil, fmt.Errorf("subscribe to provider rate-limit state: %w", err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &ProviderRateLimitProjectionWorker{cancel: cancel, done: make(chan struct{})}
	go worker.run(workerCtx, subscription, projectionStore)
	return worker, nil
}

func ensureProviderRateLimitConsumer(client *messaging.NATSClient) error {
	_, err := client.JS.AddConsumer(messaging.ProviderRateLimitKVStream(), &nats.ConsumerConfig{
		Durable: providerRateLimitConsumer, FilterSubject: messaging.ProviderRateLimitKVSubject(),
		AckPolicy: nats.AckExplicitPolicy, MaxAckPending: 2000,
	})
	if err != nil && !errors.Is(err, nats.ErrConsumerNameAlreadyInUse) {
		return fmt.Errorf("create provider rate-limit projection consumer: %w", err)
	}
	return nil
}

func (w *ProviderRateLimitProjectionWorker) Stop(ctx context.Context) {
	if w == nil {
		return
	}
	w.once.Do(w.cancel)
	select {
	case <-w.done:
	case <-ctx.Done():
	}
}

func (w *ProviderRateLimitProjectionWorker) run(ctx context.Context, subscription *nats.Subscription, projectionStore providerRateLimitProjectionStore) {
	defer close(w.done)
	for ctx.Err() == nil {
		messages, err := subscription.Fetch(providerRateLimitBatchSize, nats.MaxWait(time.Second))
		if errors.Is(err, nats.ErrTimeout) {
			continue
		}
		if err != nil {
			slog.ErrorContext(ctx, "Failed to fetch provider rate-limit state", slog.Any("error", err))
			waitForExecutionRetry(ctx)
			continue
		}
		persistProviderRateLimitMessages(ctx, projectionStore, messages)
	}
}

func persistProviderRateLimitMessages(ctx context.Context, projectionStore providerRateLimitProjectionStore, messages []*nats.Msg) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.provider_rate_limits.persist")
	defer span.End()
	states, valid := decodeProviderRateLimitMessages(messages)
	span.SetAttributes(attribute.Int("rate_limit.projection_count", len(states)))
	if len(states) == 0 {
		span.SetAttributes(attribute.String("outcome", "empty"))
		return
	}
	if err := projectionStore.BatchUpsertProviderRateLimitStates(ctx, states); err != nil {
		span.SetStatus(codes.Error, "projection_failed")
		span.SetAttributes(attribute.String("outcome", "failed"))
		nakExecutionMessages(valid)
		slog.ErrorContext(ctx, "Failed to persist provider rate-limit projection", slog.Any("error", err), slog.Int("count", len(states)))
		return
	}
	span.SetAttributes(attribute.String("outcome", "persisted"))
	ackExecutionMessages(valid)
}

func decodeProviderRateLimitMessages(messages []*nats.Msg) ([]ratelimitpolicy.StateEnvelope, []*nats.Msg) {
	states := make([]ratelimitpolicy.StateEnvelope, 0, len(messages))
	valid := make([]*nats.Msg, 0, len(messages))
	for _, message := range messages {
		var state ratelimitpolicy.StateEnvelope
		if err := json.Unmarshal(message.Data, &state); err != nil || !validProviderRateLimitState(state) {
			slog.Warn("Discarding invalid provider rate-limit state")
			_ = message.Ack()
			continue
		}
		states = append(states, state)
		valid = append(valid, message)
	}
	return states, valid
}

func validProviderRateLimitState(state ratelimitpolicy.StateEnvelope) bool {
	return state.Validate() == nil
}
