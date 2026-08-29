package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/authevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const (
	authEventWebhookConsumer  = "engine_auth_webhook_projection"
	authEventWebhookBatchSize = 100
)

// AuthEventWebhookWorker projects credential-free internal auth transitions into the existing SDK webhook transport.
type AuthEventWebhookWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// authEventWebhookMessage keeps broker disposition beside the validated event it represents.
type authEventWebhookMessage struct {
	message *nats.Msg
	event   authevent.Event
}

// authEventWebhookPayload is the public SDK body; app provenance is deliberately retained only in routing metadata.
type authEventWebhookPayload struct {
	ID               uuid.UUID      `json:"id"`
	Type             authevent.Type `json:"type"`
	OccurredAt       time.Time      `json:"occurred_at"`
	ConnectionID     uuid.UUID      `json:"connection_id"`
	ConnectSessionID *uuid.UUID     `json:"connect_session_id,omitempty"`
	BucketID         uuid.UUID      `json:"bucket_id"`
	ServiceID        uuid.UUID      `json:"service_id"`
	ServiceVersionID uuid.UUID      `json:"service_version_id"`
	EndUserRef       string         `json:"end_user_ref"`
	AuthType         string         `json:"auth_type"`
	AuthName         string         `json:"auth_name"`
	RefreshState     string         `json:"refresh_state"`
	FailureCode      string         `json:"failure_code,omitempty"`
	ResourceCount    int            `json:"resource_count,omitempty"`
}

// StartAuthEventWebhookWorker starts the single durable projector shared by all SDK receiver families.
func StartAuthEventWebhookWorker(ctx context.Context, resolver store.AuthEventAppFamilyResolver, natsClient *messaging.NATSClient) (*AuthEventWebhookWorker, error) {
	// Both durable identity resolution and JetStream are required to avoid unsafe workspace-wide fanout.
	if resolver == nil || natsClient == nil || natsClient.JS == nil {
		return nil, errors.New("auth event webhook projector dependencies are required")
	}
	// Consumer configuration must exist before the pull subscription binds to its durable identity.
	if err := ensureAuthEventWebhookConsumer(natsClient.JS); err != nil {
		return nil, err
	}
	subscription, err := natsClient.JS.PullSubscribe(
		messaging.EngineAuthEventsSubject,
		authEventWebhookConsumer,
		nats.BindStream(messaging.FusedEngineStream),
	)
	// A missing durable subscription would leave committed transitions silently unprojected.
	if err != nil {
		return nil, fmt.Errorf("subscribe to auth events for webhook projection: %w", err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &AuthEventWebhookWorker{cancel: cancel, done: make(chan struct{})}
	go worker.run(workerCtx, subscription, resolver, natsClient.JS)
	return worker, nil
}

// ensureAuthEventWebhookConsumer creates one explicit-ack consumer on the canonical internal stream.
func ensureAuthEventWebhookConsumer(js nats.JetStreamContext) error {
	_, err := js.AddConsumer(messaging.FusedEngineStream, &nats.ConsumerConfig{
		Durable:       authEventWebhookConsumer,
		FilterSubject: messaging.EngineAuthEventsSubject,
		AckPolicy:     nats.AckExplicitPolicy,
		MaxAckPending: 2000,
	})
	// An existing durable is the normal restart path; every other broker error prevents safe projection.
	if err != nil && !errors.Is(err, nats.ErrConsumerNameAlreadyInUse) {
		return fmt.Errorf("create auth event webhook consumer: %w", err)
	}
	return nil
}

// Stop cancels projector polling and waits within the caller's bounded shutdown context.
func (worker *AuthEventWebhookWorker) Stop(ctx context.Context) {
	// Optional startup wiring makes a nil worker a valid shutdown state.
	if worker == nil {
		return
	}
	worker.once.Do(worker.cancel)
	// Bounded shutdown returns when either the poller exits or the caller's shared deadline wins.
	select {
	case <-worker.done:
	case <-ctx.Done():
	}
}

// run fetches bounded batches so one set-based identity query covers every delivery attempt.
func (worker *AuthEventWebhookWorker) run(ctx context.Context, subscription *nats.Subscription, resolver store.AuthEventAppFamilyResolver, js nats.JetStreamContext) {
	defer close(worker.done)
	// Cancellation is checked before every bounded fetch so shutdown never begins another polling window.
	for ctx.Err() == nil {
		messages, err := subscription.Fetch(authEventWebhookBatchSize, nats.MaxWait(time.Second))
		// Empty polling windows are expected and should not appear as failures.
		if errors.Is(err, nats.ErrTimeout) {
			continue
		}
		// Broker failures retain pending messages and back off before the next bounded fetch.
		if err != nil {
			slog.ErrorContext(ctx, "Failed to fetch connected-auth events for SDK webhook delivery", slog.Int("batch_limit", authEventWebhookBatchSize))
			waitForExecutionRetry(ctx)
			continue
		}
		projectAuthEventWebhookMessages(ctx, resolver, js, messages)
	}
}

// projectAuthEventWebhookMessages resolves attribution once and independently advances each internal message.
func projectAuthEventWebhookMessages(ctx context.Context, resolver store.AuthEventAppFamilyResolver, js nats.JetStreamContext, messages []*nats.Msg) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.auth_events.webhook_project")
	defer span.End()
	valid, appIDs, invalidCount, unattributedCount := decodeAuthEventWebhookMessages(messages)
	span.SetAttributes(
		attribute.Int("auth_event.batch_size", len(messages)),
		attribute.Int("auth_event.valid_count", len(valid)),
		attribute.Int("auth_event.invalid_count", invalidCount),
		attribute.Int("auth_event.unattributed_count", unattributedCount),
	)
	// A batch containing only poison or CLI-created events has already reached its terminal disposition.
	if len(appIDs) == 0 {
		return
	}
	identities, err := resolver.ResolveAuthEventAppFamilies(ctx, appIDs)
	// Storage uncertainty must redeliver every valid attributed event instead of dropping it.
	if err != nil {
		span.SetStatus(codes.Error, "auth event family resolution failed")
		nakAuthEventWebhookMessages(valid)
		return
	}
	published, skipped, failed := publishAuthEventWebhookMessages(js, valid, identities)
	span.SetAttributes(
		attribute.Int("auth_event.published_count", published),
		attribute.Int("auth_event.skipped_count", skipped),
		attribute.Int("auth_event.failed_count", failed),
	)
	// Broker publication failures remain visible without attaching routing identity or payloads.
	if failed > 0 {
		span.SetStatus(codes.Error, "auth event webhook publication failed")
	}
}

// decodeAuthEventWebhookMessages validates documents and extracts unique app IDs before storage access.
func decodeAuthEventWebhookMessages(messages []*nats.Msg) ([]authEventWebhookMessage, []uuid.UUID, int, int) {
	valid := make([]authEventWebhookMessage, 0, len(messages))
	uniqueAppIDs := make(map[uuid.UUID]struct{}, len(messages))
	invalidCount := 0
	unattributedCount := 0
	// Every message reaches a terminal poison/unattributed decision or joins the one set-based resolution batch.
	for _, message := range messages {
		event, err := authevent.Decode(message.Data)
		// Malformed durable documents cannot become safe SDK events and must not redeliver forever.
		if err != nil {
			invalidCount++
			_ = message.Term()
			continue
		}
		// CLI-created connections deliberately have no app provenance and therefore no SDK recipient.
		if event.CreatedByAppID == nil {
			unattributedCount++
			_ = message.Ack()
			continue
		}
		valid = append(valid, authEventWebhookMessage{message: message, event: event})
		uniqueAppIDs[*event.CreatedByAppID] = struct{}{}
	}
	appIDs := make([]uuid.UUID, 0, len(uniqueAppIDs))
	// De-duplication keeps the resolver query cardinality tied to unique provenance rather than event count.
	for appID := range uniqueAppIDs {
		appIDs = append(appIDs, appID)
	}
	return valid, appIDs, invalidCount, unattributedCount
}

// publishAuthEventWebhookMessages republishes only SDK family events and ACKs the internal message after JetStream confirms retention.
func publishAuthEventWebhookMessages(js nats.JetStreamContext, messages []authEventWebhookMessage, identities map[uuid.UUID]store.AuthEventAppFamily) (int, int, int) {
	published := 0
	skipped := 0
	failed := 0
	// Each broker message advances independently after the single batch identity resolution.
	for _, item := range messages {
		identity, found := identities[*item.event.CreatedByAppID]
		// Missing provenance or a non-SDK family has no authorized generated receiver.
		if !found || identity.Kind != store.AppKindSDK {
			skipped++
			_ = item.message.Ack()
			continue
		}
		// Publication failure retains the internal event for a later projector retry.
		if err := publishAuthEventWebhook(js, identity, item.event); err != nil {
			failed++
			_ = item.message.NakWithDelay(time.Second)
			continue
		}
		published++
		_ = item.message.Ack()
	}
	return published, skipped, failed
}

// publishAuthEventWebhook writes one public payload to its reserved family/service webhook subject.
func publishAuthEventWebhook(js nats.JetStreamContext, identity store.AuthEventAppFamily, event authevent.Event) error {
	eventName, ok := authevent.WebhookEventName(event.Type)
	// Decode already validates the type, but fail closed if the public mapping ever narrows independently.
	if !ok {
		return errors.New("auth event has no public webhook name")
	}
	payload, err := json.Marshal(publicAuthEventWebhookPayload(event))
	// The public projection contains only fixed JSON-compatible fields, so marshal failure is treated as a retryable implementation defect.
	if err != nil {
		return fmt.Errorf("marshal auth event webhook payload: %w", err)
	}
	message := nats.NewMsg(messaging.FusedAuthWebhookSubject(identity.AccountID, identity.AppFamilyID, event.ServiceID, eventName))
	message.Data = payload
	message.Header.Set("X-Webhook-Msg-ID", event.ID.String())
	message.Header.Set(nats.MsgIdHdr, "auth-webhook:"+event.ID.String())
	// The output stream acknowledgement is the commit boundary for advancing the canonical internal event.
	if _, err := js.PublishMsg(message); err != nil {
		return fmt.Errorf("publish auth event webhook: %w", err)
	}
	return nil
}

// publicAuthEventWebhookPayload removes app provenance while preserving the connection state an SDK handler can act on.
func publicAuthEventWebhookPayload(event authevent.Event) authEventWebhookPayload {
	return authEventWebhookPayload{
		ID: event.ID, Type: event.Type, OccurredAt: event.OccurredAt,
		ConnectionID: event.ConnectionID, ConnectSessionID: event.ConnectSessionID,
		BucketID: event.BucketID, ServiceID: event.ServiceID, ServiceVersionID: event.ServiceVersionID,
		EndUserRef: event.EndUserRef, AuthType: event.AuthType, AuthName: event.AuthName,
		RefreshState: event.RefreshState, FailureCode: event.FailureCode, ResourceCount: event.ResourceCount,
	}
}

// nakAuthEventWebhookMessages schedules a bounded retry without losing batch ordering evidence.
func nakAuthEventWebhookMessages(messages []authEventWebhookMessage) {
	// Every valid attributed message shares the same transient resolution failure and retry delay.
	for _, item := range messages {
		_ = item.message.NakWithDelay(time.Second)
	}
}
