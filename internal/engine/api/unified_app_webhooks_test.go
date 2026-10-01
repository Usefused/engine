package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	natserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

type unifiedWebhookTestStore struct {
	*capabilityRouteStore
	reservationMu sync.Mutex
	target        store.UnifiedAppWebhookTarget
	deliveries    map[string]uuid.UUID
	diagnostics   json.RawMessage
}

// SaveExecutionDiagnostics retains worker evidence in the fixture so failed integration runs explain their actual failure phase.
func (s *unifiedWebhookTestStore) SaveExecutionDiagnostics(_ context.Context, _, _, _ uuid.UUID, payload json.RawMessage, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.diagnostics = append(json.RawMessage(nil), payload...)
	return nil
}

// GetExecutionDiagnostics reads the fixture's captured private evidence without involving production diagnostic authorization.
func (s *unifiedWebhookTestStore) GetExecutionDiagnostics(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, []byte) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(json.RawMessage(nil), s.diagnostics...), nil
}

// ListUnifiedAppWebhookTargets models one active family without introducing config lookups in the worker fixture.
func (s *unifiedWebhookTestStore) ListUnifiedAppWebhookTargets(context.Context) ([]store.UnifiedAppWebhookTarget, error) {
	s.reservationMu.Lock()
	defer s.reservationMu.Unlock()
	return []store.UnifiedAppWebhookTarget{s.target}, nil
}

// GetUnifiedAppWebhookTarget models the authoritative promotion pointer used before each invocation.
func (s *unifiedWebhookTestStore) GetUnifiedAppWebhookTarget(_ context.Context, familyID uuid.UUID) (*store.UnifiedAppWebhookTarget, error) {
	s.reservationMu.Lock()
	defer s.reservationMu.Unlock()
	// Removed or unrelated families have no automatic execution authority.
	if s.target.FamilyID != familyID || s.target.AppID == uuid.Nil {
		return nil, store.ErrAppRuntimeNotFound
	}
	target := s.target
	return &target, nil
}

// ReserveUnifiedAppWebhookExecution models the database's atomic event/result ownership across concurrent consumers.
func (s *unifiedWebhookTestStore) ReserveUnifiedAppWebhookExecution(ctx context.Context, record store.ExecutionResult) (uuid.UUID, bool, error) {
	s.reservationMu.Lock()
	defer s.reservationMu.Unlock()
	// A promotion racing dispatch must reject the old immutable version.
	if record.AppID != s.target.AppID {
		return uuid.Nil, false, store.ErrAppDeactivated
	}
	// The delivery fence survives both promotion and result-body deletion.
	if id, exists := s.deliveries[record.SourceWebhookEventID]; exists {
		return id, false, nil
	}
	// Failed persistence cannot consume the event's one reservation.
	if err := s.CreateExecutionResult(ctx, record); err != nil {
		return uuid.Nil, false, err
	}
	s.deliveries[record.SourceWebhookEventID] = record.ID
	return record.ID, true, nil
}

// DeleteExpiredUnifiedAppWebhookDeliveries leaves the fixture's small in-memory fence set intact for retention assertions.
func (*unifiedWebhookTestStore) DeleteExpiredUnifiedAppWebhookDeliveries(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

// newUnifiedWebhookFixture supplies real authored execution with an existing provider envelope and one selected event.
func newUnifiedWebhookFixture(t *testing.T) (*EngineGRPCServer, *unifiedWebhookTestStore, *nats.Msg) {
	t.Helper()
	_, base, familyID := newCapabilityRouteFixture(&restRuntimeTestDouble{}, nil)
	serviceID := uuid.New()
	selections, err := json.Marshal([]models.SDKSelection{{SchemaVersion: models.AppSelectionSchemaVersion,
		ServiceID: serviceID, ServiceVersionID: uuid.New(), WebhookNames: []string{"issue.created"}}})
	// Fixture scope must satisfy the production immutable-selection decoder.
	if err != nil {
		t.Fatal(err)
	}
	target := store.UnifiedAppWebhookTarget{AccountID: uuid.New(), FamilyID: familyID, AppID: base.bundle.AppID,
		Version: "1.0.0", Attachment: "team.events", ScopeSchemaVersion: models.AppScopeSchemaVersion,
		Selections: selections, CreatedAt: time.Now().Add(-time.Minute)}
	repository := &unifiedWebhookTestStore{capabilityRouteStore: base, target: target, deliveries: make(map[string]uuid.UUID)}
	base.server.store = repository
	// The authored function reads the existing body envelope while its output remains unchanged.
	base.bundle.BundleJS = strings.Replace(base.bundle.BundleJS, `if(typeof v.value!=="string")`, `/* Require the provider body before executing effects. */ if(typeof v.body.value!=="string")`, 1)
	base.bundle.BundleJS = strings.Replace(base.bundle.BundleJS, "input:{parse", "input:{/* Validate the provider envelope before granting execution. */ parse", 1)
	base.bundle.BundleJS = strings.Replace(base.bundle.BundleJS, "async execute", "/* Preserve provider body mapping inside the authored app. */ async execute", 1)
	base.bundle.BundleJS = strings.ReplaceAll(base.bundle.BundleJS, "input.value", "input.body.value")
	message := nats.NewMsg("webhooks." + target.AccountID.String() + "." + serviceID.String() + ".team-events.issue.created")
	message.Header.Set("X-Webhook-Msg-ID", uuid.NewString())
	message.Data = []byte(`{"body":{"value":"from webhook"},"headers":{},"query":{},"path":{"eventName":"issue.created"}}`)
	t.Cleanup(base.server.capabilityWorkerManager().Close)
	return base.server, repository, message
}

// TestUnifiedWebhookScopeIsolation proves subject filters and per-delivery checks preserve exact tenant, registration, and event boundaries.
func TestUnifiedWebhookScopeIsolation(t *testing.T) {
	_, repository, message := newUnifiedWebhookFixture(t)
	subjects, err := unifiedWebhookSubjects(repository.target)
	// Only the selected provider event may reach this family's consumer.
	if err != nil || len(subjects) != 1 || subjects[0] != message.Subject {
		t.Fatalf("subjects=%v err=%v", subjects, err)
	}
	// The exact selected envelope is the positive control for all denial assertions.
	if !unifiedWebhookMatches(repository.target, message) {
		t.Fatal("selected event denied")
	}
	for _, subject := range []string{
		strings.Replace(message.Subject, repository.target.AccountID.String(), uuid.NewString(), 1),
		strings.Replace(message.Subject, ".team-events.", ".other.", 1),
		strings.Replace(message.Subject, "issue.created", "issue.deleted", 1),
		messaging.FusedAuthWebhookSubject(repository.target.AccountID, repository.target.FamilyID, uuid.New(), "fused.auth.connection.completed"),
	} {
		// Negative controls differ in one authority dimension and must never run code.
		if unifiedWebhookMatches(repository.target, nats.NewMsg(subject)) {
			t.Fatalf("unauthorized subject accepted: %s", subject)
		}
	}
}

// TestUnifiedWebhookAllEventsStaysScoped proves explicit all-event scope never widens to another service or registration.
func TestUnifiedWebhookAllEventsStaysScoped(t *testing.T) {
	server, repository, message := newUnifiedWebhookFixture(t)
	var selections []models.SDKSelection
	// The fixture decoder uses the same persisted selection shape as real app publication.
	if err := json.Unmarshal(repository.target.Selections, &selections); err != nil {
		t.Fatal(err)
	}
	selections[0].WebhookNames = nil
	selections[0].WebhookSelectAll = true
	repository.target.Selections, _ = json.Marshal(selections)
	subjects, err := unifiedWebhookSubjects(repository.target)
	expected := strings.TrimSuffix(message.Subject, "issue.created") + ">"
	// All events remain underneath one exact tenant, service, and registration prefix.
	if err != nil || len(subjects) != 1 || subjects[0] != expected {
		t.Fatalf("all-event subjects=%v err=%v", subjects, err)
	}
	message.Subject = strings.ReplaceAll(message.Subject, "issue.created", "issue.deleted")
	// An explicitly broad service grant accepts a different event for that same registration.
	if !unifiedWebhookMatches(repository.target, message) {
		t.Fatal("all-event selection rejected matching registration")
	}
	repository.target.Selections = json.RawMessage(`[]`)
	_, err = server.subscribeUnifiedWebhook(repository.target)
	// Empty scope must be rejected before touching a broker, never treated as an implicit wildcard.
	if err == nil {
		t.Fatal("empty scope admitted an unfiltered consumer")
	}
}

// TestUnifiedWebhookRepairsSharedFilterDrift prevents a stale peer from permanently overwriting the promoted family's broker filters.
func TestUnifiedWebhookRepairsSharedFilterDrift(t *testing.T) {
	server, repository, message := newUnifiedWebhookFixture(t)
	js := newUnifiedWebhookJetStream(t)
	server.natsClient = &messaging.NATSClient{JS: js}
	subscription, err := server.subscribeUnifiedWebhook(repository.target)
	// Establish the normal durable before simulating another replica's stale reconciliation.
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Unsubscribe()
	info, err := js.ConsumerInfo("WEBHOOKS", unifiedWebhookConsumerName(repository.target.FamilyID))
	// The authoritative broker configuration is the baseline for the deliberate drift.
	if err != nil {
		t.Fatal(err)
	}
	info.Config.FilterSubjects = []string{strings.ReplaceAll(message.Subject, "issue.created", "issue.deleted")}
	_, err = js.UpdateConsumer("WEBHOOKS", &info.Config)
	// A stale peer can race promotion, so simulate exactly that bounded broker mutation.
	if err != nil {
		t.Fatal(err)
	}
	receivers := map[uuid.UUID]unifiedWebhookReceiver{repository.target.FamilyID: {appID: repository.target.AppID, done: make(chan struct{})}}
	server.reconcileUnifiedWebhookReceiver(context.Background(), repository, repository.target, receivers, &sync.WaitGroup{})
	info, err = js.ConsumerInfo("WEBHOOKS", unifiedWebhookConsumerName(repository.target.FamilyID))
	// Even an already-running receiver must restore current filters without needing a local restart.
	if err != nil || len(info.Config.FilterSubjects) != 1 || info.Config.FilterSubjects[0] != message.Subject {
		t.Fatalf("filters not repaired: %+v err=%v", info, err)
	}
}

// TestUnifiedWebhookExecutesOnceAndRetainsProvenance verifies real execution, event correlation, and duplicate ownership after result expiry.
func TestUnifiedWebhookExecutesOnceAndRetainsProvenance(t *testing.T) {
	requireCapabilityWorkerForDarwin(t)
	server, repository, message := newUnifiedWebhookFixture(t)
	ctx := context.Background()
	// The first event must pass through the same worker and durable completion as REST.
	if err := server.executeUnifiedWebhook(ctx, repository, repository.target.FamilyID, message); err != nil {
		t.Fatal(err)
	}
	record := repository.record
	// An automatic run records event provenance without claiming a caller execution token.
	if record.Status != "succeeded" || record.SourceWebhookEventID != message.Header.Get("X-Webhook-Msg-ID") || record.AppTokenID != uuid.Nil {
		t.Fatalf("status=%s event=%q token=%s error=%s diagnostics=%s", record.Status, record.SourceWebhookEventID, record.AppTokenID, record.ErrorCode, repository.diagnostics)
	}
	// The app receives the original envelope and can map its body into its existing return contract.
	if string(record.Output) != `{"value":"from webhook"}` {
		t.Fatalf("unexpected output: %s", record.Output)
	}
	// Expiring private result bodies must not permit an old retained message to run again.
	repository.records = make(map[uuid.UUID]store.ExecutionResult)
	if err := server.executeUnifiedWebhook(ctx, repository, repository.target.FamilyID, message); err != nil {
		t.Fatal(err)
	}
	// A duplicate owns no new result even after the old result is no longer available.
	if len(repository.records) != 0 {
		t.Fatal("redelivery restarted an expired result")
	}
	assertUnifiedWebhookReceipt(t, record, repository.target.FamilyID)
}

// assertUnifiedWebhookReceipt verifies automatic execution reuses canonical app accounting.
func assertUnifiedWebhookReceipt(t *testing.T, record store.ExecutionResult, familyID uuid.UUID) {
	t.Helper()
	receipt := unifiedAppReceipt(context.Background(), &record, capabilityRunSpec{transport: "webhook"})
	// Webhook-triggered roots keep their real transport and normal app accounting identity.
	if receipt.Transport != "webhook" || receipt.ExecutionKind != "unified" || receipt.AppFamilyID != familyID {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	// Both direct reads and search results must retain the event-to-execution link.
	if publicExecutionResult(&record).SourceWebhookEventID != record.SourceWebhookEventID {
		t.Fatal("public result lost source event provenance")
	}
}

// TestUnifiedWebhookInvalidAndRemovedTargets proves admission rejects malformed envelopes and never executes deactivated versions.
func TestUnifiedWebhookInvalidAndRemovedTargets(t *testing.T) {
	server, repository, message := newUnifiedWebhookFixture(t)
	message.Header.Del("X-Webhook-Msg-ID")
	// Missing stable identity cannot be replaced by a new random ID on each retry.
	if err := server.executeUnifiedWebhook(context.Background(), repository, repository.target.FamilyID, message); !errors.Is(err, store.ErrExecutionResultInvalid) {
		t.Fatalf("missing event ID: %v", err)
	}
	repository.target.AppID = uuid.Nil
	// A removed target consumes no execution capacity and does not create a retry loop.
	if err := server.executeUnifiedWebhook(context.Background(), repository, repository.target.FamilyID, message); err != nil {
		t.Fatal(err)
	}
	// Both rejection paths must precede a durable execution reservation.
	if len(repository.records) != 0 {
		t.Fatal("invalid or deactivated event executed")
	}
}

// newUnifiedWebhookJetStream uses a real local broker to verify durable filter changes and cursor continuity.
func newUnifiedWebhookJetStream(t *testing.T) nats.JetStreamContext {
	t.Helper()
	server, err := natserver.NewServer(&natserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	// The fixture must own a usable ephemeral server before subscribing.
	if err != nil {
		t.Fatal(err)
	}
	go server.Start()
	t.Cleanup(server.Shutdown)
	// Readiness makes subsequent consumer errors attributable to the behavior under test.
	if !server.ReadyForConnections(10 * time.Second) {
		t.Fatal("NATS startup timed out")
	}
	connection, err := nats.Connect(server.ClientURL())
	// Fixture connection failures cannot be mistaken for subscription assertions.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(connection.Close)
	js, err := connection.JetStream()
	// The tested adapter requires the existing JetStream transport.
	if err != nil {
		t.Fatal(err)
	}
	_, err = js.AddStream(&nats.StreamConfig{Name: "WEBHOOKS", Subjects: []string{"webhooks.>"}, MaxAge: 30 * 24 * time.Hour})
	// Match production retention so delivery fence assumptions are exercised alongside real consumer setup.
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// TestUnifiedWebhookDurablePromotion preserves pending delivery while replacing selected events and reconnecting.
func TestUnifiedWebhookDurablePromotion(t *testing.T) {
	server, repository, message := newUnifiedWebhookFixture(t)
	js := newUnifiedWebhookJetStream(t)
	server.natsClient = &messaging.NATSClient{JS: js}
	first, err := server.subscribeUnifiedWebhook(repository.target)
	// First deployment creates one family-owned durable subscription.
	if err != nil {
		t.Fatal(err)
	}
	defer first.Unsubscribe()
	_, err = js.PublishMsg(message)
	// Publishing through JetStream ensures pending acknowledgements are real broker state.
	if err != nil {
		t.Fatal(err)
	}
	batch, err := first.Fetch(1, nats.MaxWait(2*time.Second))
	// The selected event must arrive before promotion.
	if err != nil || len(batch) != 1 {
		t.Fatalf("initial fetch: %v", err)
	}
	// A negative acknowledgement models interrupted delivery without inventing a new event.
	if err := batch[0].Nak(); err != nil {
		t.Fatal(err)
	}
	_ = first.Unsubscribe()
	repository.target.AppID = uuid.New()
	repository.target.Version = "2.0.0"
	second, err := server.subscribeUnifiedWebhook(repository.target)
	// A promoted version reuses the family's existing cursor.
	if err != nil {
		t.Fatal(err)
	}
	defer second.Unsubscribe()
	assertUnifiedWebhookPendingDelivery(t, second, js, repository.target.FamilyID, message.Header.Get("X-Webhook-Msg-ID"))
	_ = second.Unsubscribe()
	assertUnifiedWebhookFilterUpdate(t, server, repository, js, message)
}

// assertUnifiedWebhookPendingDelivery verifies reconnect and acknowledgement preserve a family durable cursor.
func assertUnifiedWebhookPendingDelivery(t *testing.T, second *nats.Subscription, js nats.JetStreamContext, familyID uuid.UUID, eventID string) {
	t.Helper()
	batch, err := second.Fetch(1, nats.MaxWait(2*time.Second))
	// Pending delivery must survive reconnect and keep its original event ID.
	if err != nil || len(batch) != 1 || batch[0].Header.Get("X-Webhook-Msg-ID") != eventID {
		t.Fatalf("redelivery: %v", err)
	}
	// Explicit ack settles the event rather than callback completion.
	if err := batch[0].AckSync(); err != nil {
		t.Fatal(err)
	}
	info, err := js.ConsumerInfo("WEBHOOKS", unifiedWebhookConsumerName(familyID))
	// Promotion cannot leave a second durable or reset acknowledged progress.
	if err != nil || info.NumAckPending != 0 || info.AckFloor.Stream == 0 {
		t.Fatalf("consumer state=%+v err=%v", info, err)
	}
}

// assertUnifiedWebhookFilterUpdate checks that promotion replaces the finite event filter without replaying removed events.
func assertUnifiedWebhookFilterUpdate(t *testing.T, server *EngineGRPCServer, repository *unifiedWebhookTestStore, js nats.JetStreamContext, message *nats.Msg) {
	t.Helper()
	repository.target.Selections = json.RawMessage(strings.ReplaceAll(string(repository.target.Selections), "issue.created", "issue.updated"))
	third, err := server.subscribeUnifiedWebhook(repository.target)
	// New immutable event selections update the existing durable instead of creating a second cursor.
	if err != nil {
		t.Fatal(err)
	}
	defer third.Unsubscribe()
	_, err = js.PublishMsg(message)
	// The removed event is retained in the shared stream but must no longer reach this app.
	if err != nil {
		t.Fatal(err)
	}
	message.Subject = strings.ReplaceAll(message.Subject, "issue.created", "issue.updated")
	_, err = js.PublishMsg(message)
	// The newly selected event must reach the promoted consumer using the same registration.
	if err != nil {
		t.Fatal(err)
	}
	batch, err := third.Fetch(1, nats.MaxWait(2*time.Second))
	// Selection changes cannot broaden delivery back to the removed event.
	if err != nil || len(batch) != 1 || batch[0].Subject != message.Subject {
		t.Fatalf("updated filter delivery: %v", err)
	}
	_ = batch[0].AckSync()
}

// TestUnifiedWebhookWorkerRunsAndStops exercises broker-to-worker wiring with real authored execution and bounded shutdown.
func TestUnifiedWebhookWorkerRunsAndStops(t *testing.T) {
	requireCapabilityWorkerForDarwin(t)
	server, repository, message := newUnifiedWebhookFixture(t)
	js := newUnifiedWebhookJetStream(t)
	server.natsClient = &messaging.NATSClient{JS: js}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	worker, err := server.StartUnifiedAppWebhookWorker(ctx)
	// Starting the real worker proves the production server has every required dependency.
	if err != nil {
		t.Fatal(err)
	}
	// Always join the pull loop before the fixture closes its broker and execution manager.
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		worker.Stop(stopCtx)
	})
	_, err = js.PublishMsg(message)
	// Publication may precede initial discovery; the consumer's start time must still include this event.
	if err != nil {
		t.Fatal(err)
	}
	record := waitUnifiedWebhookResult(t, repository)
	// The broker invocation must complete through the normal hosted app pipeline.
	if record.Status != "succeeded" || string(record.Output) != `{"value":"from webhook"}` {
		t.Fatalf("status=%s output=%s error=%s", record.Status, record.Output, record.ErrorCode)
	}
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	worker.Stop(stopCtx)
	// Shutdown must actually join the consumer, not merely send cancellation.
	select {
	case <-worker.done:
	default:
		t.Fatal("webhook worker did not stop")
	}
}

// waitUnifiedWebhookResult observes durable fixture state without racing the background consumer.
func waitUnifiedWebhookResult(t *testing.T, repository *unifiedWebhookTestStore) store.ExecutionResult {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		repository.mu.Lock()
		record := repository.record
		repository.mu.Unlock()
		// Terminal persistence is the signal that both execution and provider-capable work are complete.
		if record.CompletedAt != nil {
			return record
		}
		select {
		case <-deadline.C:
			t.Fatal("webhook execution did not complete")
			return store.ExecutionResult{}
		case <-ticker.C:
		}
	}
}
