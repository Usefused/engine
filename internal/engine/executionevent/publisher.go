package executionevent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var ErrPublisherUnavailable = errors.New("execution event publisher unavailable")

type jetStreamPublisher interface {
	PublishMsgJS(msg *nats.Msg) (*nats.PubAck, error)
}

type Publisher struct {
	js jetStreamPublisher
}

type WebhookEventInput struct {
	MessageID          string
	AccountID          uuid.UUID
	ServiceID          uuid.UUID
	ServiceVersionID   uuid.UUID
	RegistrationID     uuid.UUID
	EventName          string
	DeliveryStatus     string
	VerificationStatus string
	FailureReason      string
	Environment        string
	PayloadSize        int64
	LatencyMs          int64
	AttemptCount       int
	OccurredAt         time.Time
}

func NewWebhookEvent(input WebhookEventInput) models.EngineExecutionEvent {
	occurredAt := input.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	status, category, code := webhookOutcome(input.DeliveryStatus)
	return models.EngineExecutionEvent{
		ID:                 WebhookEventID(input.MessageID),
		AccountID:          input.AccountID,
		Transport:          models.EngineExecutionTransportWebhook,
		Direction:          models.EngineExecutionDirectionInbound,
		ServiceID:          input.ServiceID,
		ServiceVersionID:   input.ServiceVersionID.String(),
		WebhookID:          input.RegistrationID,
		EndpointName:       input.EventName,
		ExternalID:         input.MessageID,
		EventName:          input.EventName,
		Environment:        input.Environment,
		Status:             status,
		FailureReason:      input.FailureReason,
		FailureCategory:    category,
		FailureCode:        code,
		LatencyMs:          input.LatencyMs,
		AttemptCount:       input.AttemptCount,
		RequestBytes:       input.PayloadSize,
		VerificationStatus: input.VerificationStatus,
		DeliveryStatus:     input.DeliveryStatus,
		StartedAt:          occurredAt.Add(-time.Duration(input.LatencyMs) * time.Millisecond),
		EndedAt:            occurredAt,
		CreatedAt:          occurredAt,
	}
}

func WebhookEventID(messageID string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("fused-webhook:"+messageID))
}

func webhookOutcome(deliveryStatus string) (status, category, code string) {
	switch deliveryStatus {
	case "failed":
		return models.EngineExecutionStatusFailed, "delivery", "webhook_delivery_failed"
	case "rejected":
		return models.EngineExecutionStatusFailed, "verification", "webhook_rejected"
	default:
		return models.EngineExecutionStatusSuccess, "", ""
	}
}

func NewPublisher(js jetStreamPublisher) *Publisher {
	if js == nil {
		return nil
	}
	return &Publisher{js: js}
}

func (p *Publisher) Publish(ctx context.Context, event models.EngineExecutionEvent) error {
	event = normalize(event)
	if err := validate(event); err != nil {
		recordPublish(ctx, event, err)
		return err
	}
	payload, err := json.Marshal(models.EngineExecutionEventEnvelope{
		SchemaVersion: models.EngineExecutionEventSchemaVersion,
		Event:         event,
	})
	if err != nil {
		recordPublish(ctx, event, err)
		return fmt.Errorf("marshal execution event: %w", err)
	}
	msg := nats.NewMsg(messaging.EngineExecutionEventsSubject)
	msg.Data = payload
	// JetStream uses this ID to suppress a rapid duplicate publish before the
	// database's primary key provides the final idempotency boundary.
	msg.Header.Set(nats.MsgIdHdr, event.ID.String())
	_, err = p.js.PublishMsgJS(msg)
	recordPublish(ctx, event, err)
	if err != nil {
		return fmt.Errorf("publish execution event: %w", err)
	}
	return nil
}

func normalize(event models.EngineExecutionEvent) models.EngineExecutionEvent {
	if event.ID == uuid.Nil {
		event.ID = uuid.New()
	}
	if event.Direction == "" {
		event.Direction = models.EngineExecutionDirectionOutbound
	}
	if event.AttemptCount <= 0 {
		event.AttemptCount = 1
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	return event
}

func validate(event models.EngineExecutionEvent) error {
	if event.Transport == "" {
		return errors.New("execution event transport is required")
	}
	if err := validateAppIdentity(event); err != nil {
		return err
	}
	if event.Status != models.EngineExecutionStatusSuccess && event.Status != models.EngineExecutionStatusFailed {
		return fmt.Errorf("invalid execution event status %q", event.Status)
	}
	if event.StartedAt.IsZero() || event.EndedAt.IsZero() {
		return errors.New("execution event timestamps are required")
	}
	return nil
}

func validateAppIdentity(event models.EngineExecutionEvent) error {
	if !appTransport(event.Transport) {
		return nil
	}
	if event.AppFamilyID == uuid.Nil || event.AppID == uuid.Nil || event.AppVersion == "" {
		return errors.New("SDK and MCP execution events require app family, app, and version identity")
	}
	if len(event.AppVersion) > 128 {
		return errors.New("execution event app version is too long")
	}
	return nil
}

func appTransport(transport string) bool {
	return transport == models.EngineExecutionTransportSDK || transport == models.EngineExecutionTransportMCP
}

func recordPublish(ctx context.Context, event models.EngineExecutionEvent, publishErr error) {
	span := trace.SpanFromContext(ctx)
	attrs := []attribute.KeyValue{
		attribute.String("execution.event_id", event.ID.String()),
		attribute.String("execution.transport", event.Transport),
		attribute.String("execution.direction", event.Direction),
		attribute.String("execution.status", event.Status),
	}
	if event.ServiceID != uuid.Nil {
		attrs = append(attrs, attribute.String("service.id", event.ServiceID.String()))
	}
	if event.AppFamilyID != uuid.Nil {
		attrs = append(attrs,
			attribute.String("app.family_id", event.AppFamilyID.String()),
			attribute.String("app.id", event.AppID.String()),
			attribute.String("app.version", event.AppVersion),
		)
	}
	if publishErr != nil {
		span.RecordError(publishErr)
		span.SetStatus(codes.Error, "execution event publication failed")
		attrs = append(attrs, attribute.Bool("execution.persist_queued", false))
	} else {
		attrs = append(attrs, attribute.Bool("execution.persist_queued", true))
	}
	span.AddEvent("engine.execution.event", trace.WithAttributes(attrs...))
}

var globalPublisher struct {
	sync.RWMutex
	publisher *Publisher
}

func SetPublisher(publisher *Publisher) {
	globalPublisher.Lock()
	globalPublisher.publisher = publisher
	globalPublisher.Unlock()
}

// Publish is the sole entry point used by runtime execution paths. Keeping the
// global wiring here prevents SDK, MCP, and webhook packages from owning their
// own persistence mechanisms while boot still controls the concrete publisher.
func Publish(ctx context.Context, event models.EngineExecutionEvent) error {
	globalPublisher.RLock()
	publisher := globalPublisher.publisher
	globalPublisher.RUnlock()
	if publisher == nil {
		err := ErrPublisherUnavailable
		recordPublish(ctx, event, err)
		return err
	}
	return publisher.Publish(ctx, event)
}
