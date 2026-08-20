package executionevent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

type publisherStub struct {
	message *nats.Msg
	err     error
}

func (s *publisherStub) PublishMsgJS(message *nats.Msg) (*nats.PubAck, error) {
	s.message = message
	return &nats.PubAck{}, s.err
}

func TestPublisherNormalizesAndVersionsEvent(t *testing.T) {
	stub := &publisherStub{}
	publisher := NewPublisher(stub)
	now := time.Now()
	event := models.EngineExecutionEvent{
		Transport: models.EngineExecutionTransportSDK, Status: models.EngineExecutionStatusSuccess,
		AppFamilyID: uuid.New(), AppID: uuid.New(), AppVersion: "1.0.0",
		ProviderProtocol: models.ProviderProtocolREST,
		StartedAt:        now.Add(-time.Millisecond), EndedAt: now,
	}
	if err := publisher.Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	var envelope models.EngineExecutionEventEnvelope
	if err := json.Unmarshal(stub.message.Data, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != models.EngineExecutionEventSchemaVersion {
		t.Fatalf("schema version = %d", envelope.SchemaVersion)
	}
	if envelope.Event.ID == uuid.Nil || envelope.Event.AttemptCount != 1 {
		t.Fatalf("event was not normalized: %#v", envelope.Event)
	}
	if stub.message.Header.Get(nats.MsgIdHdr) != envelope.Event.ID.String() {
		t.Fatal("NATS de-duplication ID must match the event ID")
	}
}

func TestPublisherReturnsJetStreamFailure(t *testing.T) {
	publisher := NewPublisher(&publisherStub{err: errors.New("unavailable")})
	now := time.Now()
	err := publisher.Publish(context.Background(), models.EngineExecutionEvent{
		Transport: models.EngineExecutionTransportSDK, Status: models.EngineExecutionStatusSuccess,
		AppFamilyID: uuid.New(), AppID: uuid.New(), AppVersion: "1.0.0",
		ProviderProtocol: models.ProviderProtocolREST,
		StartedAt:        now.Add(-time.Millisecond), EndedAt: now,
	})
	if err == nil {
		t.Fatal("expected publication failure")
	}
}

func TestPublisherRejectsSDKEventWithoutVersionIdentity(t *testing.T) {
	now := time.Now()
	err := NewPublisher(&publisherStub{}).Publish(context.Background(), models.EngineExecutionEvent{
		Transport: models.EngineExecutionTransportSDK, Status: models.EngineExecutionStatusSuccess,
		StartedAt: now.Add(-time.Millisecond), EndedAt: now,
	})
	if err == nil {
		t.Fatal("expected SDK identity validation failure")
	}
}

// TestPublisherTreatsRESTAsAppTransport proves REST receipts require the same
// immutable identity as generated SDK and MCP calls.
func TestPublisherTreatsRESTAsAppTransport(t *testing.T) {
	now := time.Now()
	err := NewPublisher(&publisherStub{}).Publish(context.Background(), models.EngineExecutionEvent{
		Transport: models.EngineExecutionTransportREST, Status: models.EngineExecutionStatusSuccess,
		StartedAt: now.Add(-time.Millisecond), EndedAt: now,
	})
	if err == nil {
		t.Fatal("REST event without app identity was accepted")
	}
}

func TestWebhookEventUsesStableMessageIdentity(t *testing.T) {
	input := WebhookEventInput{MessageID: "message-1", DeliveryStatus: "success", OccurredAt: time.Now()}
	first := NewWebhookEvent(input)
	second := NewWebhookEvent(input)
	if first.ID != second.ID {
		t.Fatalf("webhook event IDs differ: %s != %s", first.ID, second.ID)
	}
}
