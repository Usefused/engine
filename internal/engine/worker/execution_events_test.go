package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

type captureExecutionEventStore struct {
	events         []models.EngineExecutionEvent
	aggregateUsage []bool
	err            error
}

type retryExecutionEventStore struct {
	attempts  atomic.Int32
	persisted chan uuid.UUID
}

// BatchCreateEngineExecutionEventsAndUsage captures the atomic store request and its live accounting mode.
func (s *captureExecutionEventStore) BatchCreateEngineExecutionEventsAndUsage(_ context.Context, events []models.EngineExecutionEvent, aggregateUsage bool) error {
	s.events = append(s.events, events...)
	s.aggregateUsage = append(s.aggregateUsage, aggregateUsage)
	return s.err
}

// BatchCreateEngineExecutionEventsAndUsage fails once so a real JetStream consumer must redeliver the atomic receipt-and-usage request.
func (s *retryExecutionEventStore) BatchCreateEngineExecutionEventsAndUsage(_ context.Context, events []models.EngineExecutionEvent, aggregateUsage bool) error {
	attempt := s.attempts.Add(1)
	// The first database failure must leave the stream delivery pending instead of losing its usage increment.
	if attempt == 1 {
		return errors.New("database unavailable")
	}
	// A successful retry must still carry both the receipt identity and enabled accounting mode.
	if len(events) != 1 || !aggregateUsage {
		return errors.New("invalid redelivery payload")
	}
	s.persisted <- events[0].ID
	return nil
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

// TestPersistExecutionMessagesWritesValidBatch keeps receipt and entitlement mode in one store boundary.
func TestPersistExecutionMessagesWritesValidBatch(t *testing.T) {
	first := executionMessage(t, validExecutionEvent())
	secondEvent := validExecutionEvent()
	secondEvent.ID = uuid.New()
	second := executionMessage(t, secondEvent)
	store := &captureExecutionEventStore{}

	persistExecutionMessages(context.Background(), store, []*nats.Msg{first, second}, true)

	if len(store.events) != 2 {
		t.Fatalf("persisted %d events, want 2", len(store.events))
	}
	// The worker must forward the entitlement decision to the same atomic store call as the receipts.
	if len(store.aggregateUsage) != 1 || !store.aggregateUsage[0] {
		t.Fatalf("aggregate usage decisions = %#v, want [true]", store.aggregateUsage)
	}
}

// TestPersistExecutionMessagesDoesNotHideStoreFailure keeps failed durable writes eligible for stream retry.
func TestPersistExecutionMessagesDoesNotHideStoreFailure(t *testing.T) {
	store := &captureExecutionEventStore{err: errors.New("database unavailable")}
	persistExecutionMessages(context.Background(), store, []*nats.Msg{executionMessage(t, validExecutionEvent())}, true)
	if len(store.events) != 1 {
		t.Fatalf("store received %d events, want 1", len(store.events))
	}
}

// TestExecutionEventWorkerRedeliversAtomicUsageAfterStoreFailure proves the durable stream replaces the retired lossy memory queue.
func TestExecutionEventWorkerRedeliversAtomicUsageAfterStoreFailure(t *testing.T) {
	natsClient := executionEventTestNATSClient(t)
	// The worker consumer can exist only after its canonical internal stream has been initialized.
	if err := natsClient.InitStream(messaging.FusedEngineStream, messaging.FusedEngineStreamSubjects()); err != nil {
		t.Fatalf("initialize execution event stream: %v", err)
	}
	store := &retryExecutionEventStore{persisted: make(chan uuid.UUID, 1)}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	worker, err := StartExecutionEventWorker(workerCtx, store, natsClient, func() bool { return true })
	// Startup failure would leave the test unable to exercise broker redelivery.
	if err != nil {
		cancelWorker()
		t.Fatalf("start execution event worker: %v", err)
	}
	t.Cleanup(func() {
		// Bounded cleanup prevents a broken consumer from hanging the package test.
		cancelWorker()
		stopCtx, cancelStop := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancelStop()
		worker.Stop(stopCtx)
	})

	event := validExecutionEvent()
	// Publication through the production envelope path preserves the event ID and message headers used in deployment.
	if err := executionevent.NewPublisher(natsClient).Publish(context.Background(), event); err != nil {
		t.Fatalf("publish execution event: %v", err)
	}
	select {
	case persistedID := <-store.persisted:
		// Redelivery must preserve the stable receipt ID used by the database accounting ledger.
		if persistedID != event.ID {
			t.Fatalf("persisted event ID = %s, want %s", persistedID, event.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execution event was not redelivered after the store failure")
	}
	waitForExecutionEventConsumerAck(t, natsClient.JS)
	// One failed attempt followed by one successful attempt proves there was no best-effort drop.
	if attempts := store.attempts.Load(); attempts != 2 {
		t.Fatalf("store attempts = %d, want 2", attempts)
	}
}

// executionEventTestNATSClient starts an isolated file-backed broker for receipt redelivery tests.
func executionEventTestNATSClient(t *testing.T) *messaging.NATSClient {
	t.Helper()
	natsServer, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	// Broker construction failure cannot fall back to an in-memory fake for acknowledgement semantics.
	if err != nil {
		t.Fatalf("create NATS server: %v", err)
	}
	go natsServer.Start()
	// Ten seconds accommodates loaded CI hosts without weakening the bounded readiness guarantee.
	if !natsServer.ReadyForConnections(10 * time.Second) {
		t.Fatal("NATS server did not become ready")
	}
	t.Cleanup(natsServer.Shutdown)
	connection, err := nats.Connect(natsServer.ClientURL())
	// The worker must use a real connection so ACK and NAK change durable consumer state.
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(connection.Close)
	jetStream, err := connection.JetStream()
	// Core NATS cannot prove redelivery or explicit acknowledgement semantics.
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return &messaging.NATSClient{Conn: connection, JS: jetStream}
}

// waitForExecutionEventConsumerAck waits until the real durable consumer has acknowledged its successful retry.
func waitForExecutionEventConsumerAck(t *testing.T, jetStream nats.JetStreamContext) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := jetStream.ConsumerInfo(messaging.FusedEngineStream, executionEventConsumer)
		if err != nil {
			t.Fatalf("read execution event consumer: %v", err)
		}
		// Both counters reach zero only after the worker has ACKed the committed store call.
		if info.NumAckPending == 0 && info.NumPending == 0 {
			return
		}
		// A bounded deadline turns missing ACKs into deterministic test failures.
		if time.Now().After(deadline) {
			t.Fatalf("consumer pending=%d ack_pending=%d", info.NumPending, info.NumAckPending)
		}
		time.Sleep(10 * time.Millisecond)
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

// validExecutionEvent returns one final physical receipt suitable for durable worker tests.
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
