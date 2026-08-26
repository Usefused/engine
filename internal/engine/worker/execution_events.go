package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const (
	executionEventConsumer  = "engine_execution_postgres"
	executionEventBatchSize = 100
)

type executionEventStore interface {
	BatchCreateEngineExecutionEvents(ctx context.Context, events []models.EngineExecutionEvent) error
}

type ExecutionEventWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func StartExecutionEventWorker(ctx context.Context, eventStore executionEventStore, natsClient *messaging.NATSClient) (*ExecutionEventWorker, error) {
	if eventStore == nil || natsClient == nil || natsClient.JS == nil {
		return nil, errorsForExecutionWorker(eventStore, natsClient)
	}
	if err := ensureExecutionConsumer(natsClient); err != nil {
		return nil, err
	}
	subscription, err := natsClient.JS.PullSubscribe(
		messaging.EngineExecutionEventsSubject,
		executionEventConsumer,
		nats.BindStream(messaging.FusedEngineStream),
	)
	if err != nil {
		return nil, fmt.Errorf("subscribe to execution events: %w", err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &ExecutionEventWorker{cancel: cancel, done: make(chan struct{})}
	go worker.run(workerCtx, subscription, eventStore)
	return worker, nil
}

func errorsForExecutionWorker(eventStore executionEventStore, natsClient *messaging.NATSClient) error {
	if eventStore == nil {
		return fmt.Errorf("execution event store is required")
	}
	if natsClient == nil || natsClient.JS == nil {
		return fmt.Errorf("execution event JetStream client is required")
	}
	return nil
}

func ensureExecutionConsumer(natsClient *messaging.NATSClient) error {
	_, err := natsClient.JS.AddConsumer(messaging.FusedEngineStream, &nats.ConsumerConfig{
		Durable:       executionEventConsumer,
		FilterSubject: messaging.EngineExecutionEventsSubject,
		AckPolicy:     nats.AckExplicitPolicy,
		MaxAckPending: 2000,
	})
	if err != nil && err != nats.ErrConsumerNameAlreadyInUse {
		return fmt.Errorf("create execution event consumer: %w", err)
	}
	return nil
}

func (w *ExecutionEventWorker) Stop(ctx context.Context) {
	if w == nil {
		return
	}
	w.once.Do(w.cancel)
	select {
	case <-w.done:
	case <-ctx.Done():
	}
}

func (w *ExecutionEventWorker) run(ctx context.Context, subscription *nats.Subscription, eventStore executionEventStore) {
	defer close(w.done)
	for ctx.Err() == nil {
		messages, err := subscription.Fetch(executionEventBatchSize, nats.MaxWait(time.Second))
		if err == nats.ErrTimeout {
			continue
		}
		if err != nil {
			slog.ErrorContext(ctx, "Failed to fetch execution events", slog.Any("error", err))
			waitForExecutionRetry(ctx)
			continue
		}
		persistExecutionMessages(ctx, eventStore, messages)
	}
}

func waitForExecutionRetry(ctx context.Context) {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func persistExecutionMessages(ctx context.Context, eventStore executionEventStore, messages []*nats.Msg) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_events.persist")
	defer span.End()
	events, validMessages := decodeExecutionMessages(messages)
	span.SetAttributes(attribute.Int("execution.batch_size", len(events)))
	if len(events) == 0 {
		return
	}
	if err := eventStore.BatchCreateEngineExecutionEvents(ctx, events); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "execution event persistence failed")
		nakExecutionMessages(validMessages)
		slog.ErrorContext(ctx, "Failed to persist execution events", slog.Any("error", err), slog.Int("count", len(events)))
		return
	}
	ackExecutionMessages(validMessages)
}

func decodeExecutionMessages(messages []*nats.Msg) ([]models.EngineExecutionEvent, []*nats.Msg) {
	events := make([]models.EngineExecutionEvent, 0, len(messages))
	validMessages := make([]*nats.Msg, 0, len(messages))
	for _, message := range messages {
		event, err := decodeExecutionMessage(message.Data)
		if err != nil {
			slog.Warn("Discarding invalid execution event", slog.Any("error", err))
			_ = message.Ack()
			continue
		}
		events = append(events, event)
		validMessages = append(validMessages, message)
	}
	return events, validMessages
}

// decodeExecutionMessage admits the current receipt contract and in-flight pre-hierarchy events.
func decodeExecutionMessage(payload []byte) (models.EngineExecutionEvent, error) {
	var envelope models.EngineExecutionEventEnvelope
	// Corrupt envelopes are poison messages, not transient database failures.
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return models.EngineExecutionEvent{}, fmt.Errorf("decode execution event: %w", err)
	}
	// Version five events remain valid during rolling upgrades and durable queue replay.
	if envelope.SchemaVersion != models.EngineExecutionEventSchemaVersion && envelope.SchemaVersion != 5 {
		return models.EngineExecutionEvent{}, fmt.Errorf("unsupported execution event schema version %d", envelope.SchemaVersion)
	}
	// Every durable event needs its own idempotency identity.
	if envelope.Event.ID.String() == "00000000-0000-0000-0000-000000000000" {
		return models.EngineExecutionEvent{}, fmt.Errorf("execution event id is required")
	}
	// Reject invalid hierarchy metadata before a malformed message can poison a whole store batch.
	if err := executionevent.ValidateUnifiedMetadata(envelope.Event); err != nil {
		return models.EngineExecutionEvent{}, err
	}
	return envelope.Event, nil
}

func ackExecutionMessages(messages []*nats.Msg) {
	for _, message := range messages {
		_ = message.Ack()
	}
}

func nakExecutionMessages(messages []*nats.Msg) {
	for _, message := range messages {
		_ = message.Nak()
	}
}
