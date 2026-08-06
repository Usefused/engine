package worker

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

type captureExecutionEventStore struct {
	events []models.EngineExecutionEvent
	err    error
}

func (s *captureExecutionEventStore) BatchCreateEngineExecutionEvents(_ context.Context, events []models.EngineExecutionEvent) error {
	s.events = append(s.events, events...)
	return s.err
}

func TestDecodeExecutionMessageRejectsUnknownSchema(t *testing.T) {
	payload, err := json.Marshal(models.EngineExecutionEventEnvelope{SchemaVersion: 99, Event: validExecutionEvent()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeExecutionMessage(payload); err == nil {
		t.Fatal("expected an unsupported schema version error")
	}
}

func TestPersistExecutionMessagesWritesValidBatch(t *testing.T) {
	first := executionMessage(t, validExecutionEvent())
	secondEvent := validExecutionEvent()
	secondEvent.ID = uuid.New()
	second := executionMessage(t, secondEvent)
	store := &captureExecutionEventStore{}

	persistExecutionMessages(context.Background(), store, []*nats.Msg{first, second})

	if len(store.events) != 2 {
		t.Fatalf("persisted %d events, want 2", len(store.events))
	}
}

func TestPersistExecutionMessagesDoesNotHideStoreFailure(t *testing.T) {
	store := &captureExecutionEventStore{err: errors.New("database unavailable")}
	persistExecutionMessages(context.Background(), store, []*nats.Msg{executionMessage(t, validExecutionEvent())})
	if len(store.events) != 1 {
		t.Fatalf("store received %d events, want 1", len(store.events))
	}
}

func executionMessage(t *testing.T, event models.EngineExecutionEvent) *nats.Msg {
	t.Helper()
	payload, err := json.Marshal(models.EngineExecutionEventEnvelope{
		SchemaVersion: models.EngineExecutionEventSchemaVersion,
		Event:         event,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := nats.NewMsg("engine.execution.events.v1")
	message.Data = payload
	return message
}

func validExecutionEvent() models.EngineExecutionEvent {
	now := time.Now()
	return models.EngineExecutionEvent{
		ID: uuid.New(), Transport: models.EngineExecutionTransportSDK,
		AppFamilyID: uuid.New(), AppID: uuid.New(), AppVersion: "1.0.0",
		ProviderProtocol: models.ProviderProtocolREST,
		Direction:        models.EngineExecutionDirectionOutbound, Status: models.EngineExecutionStatusSuccess,
		StartedAt: now.Add(-time.Millisecond), EndedAt: now, CreatedAt: now,
	}
}
