package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/authevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/google/uuid"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// authEventResolverStub returns a pre-authorized family map without weakening the worker's set-based interface.
type authEventResolverStub struct {
	identities map[uuid.UUID]store.AuthEventAppFamily
}

// assertProjectedAuthWebhook checks the stable public identity and absence of internal provenance.
func assertProjectedAuthWebhook(t *testing.T, message *nats.Msg, event authevent.Event, serviceID uuid.UUID) {
	t.Helper()
	// Stable event identity gives generated receivers idempotent ACK correlation.
	if message.Header.Get("X-Webhook-Msg-ID") != event.ID.String() {
		t.Fatalf("message ID = %q, want %q", message.Header.Get("X-Webhook-Msg-ID"), event.ID)
	}
	var payload map[string]any
	// Public payload must remain valid JSON after internal provenance removal.
	if err := json.Unmarshal(message.Data, &payload); err != nil {
		t.Fatalf("decode public payload: %v", err)
	}
	// Actionable connection and service selectors remain available to SDK handlers.
	if payload["end_user_ref"] != "customer-42" || payload["service_id"] != serviceID.String() {
		t.Fatalf("public payload = %#v", payload)
	}
	// Internal app provenance is used only for routing and must not reach the public handler.
	if _, leaked := payload["created_by_app_id"]; leaked {
		t.Fatal("public payload leaked internal app provenance")
	}
}

// assertReconnectRequiredPayload verifies actionable failure state while denying credential and routing leakage.
func assertReconnectRequiredPayload(t *testing.T, payloadJSON []byte, failureCode string) map[string]any {
	t.Helper()
	var payload map[string]any
	// Failure fields must be actionable without exposing provider response or token material.
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("decode reconnect-required payload: %v", err)
	}
	// The caller supplies the stable failure code so provider rejection and missing refresh material share one payload contract.
	if payload["type"] != "connection.reconnect_required" || payload["refresh_state"] != "reconnect_required" || payload["failure_code"] != failureCode {
		t.Fatalf("reconnect-required payload = %#v", payload)
	}
	assertAuthEventPayloadSafe(t, payload)
	return payload
}

// assertTokenRefreshedPayload verifies the healthy public state without admitting credential or routing fields.
func assertTokenRefreshedPayload(t *testing.T, payloadJSON []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	// Successful rotation must remain machine-readable through the generated SDK receiver.
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("decode token-refreshed payload: %v", err)
	}
	// A healthy event cannot retain a failure code from an earlier refresh attempt.
	if payload["type"] != "token.refreshed" || payload["refresh_state"] != "ok" || payload["failure_code"] != nil {
		t.Fatalf("token-refreshed payload = %#v", payload)
	}
	assertAuthEventPayloadSafe(t, payload)
	return payload
}

// assertAuthEventPayloadSafe centralizes the public lifecycle event credential and provenance denylist.
func assertAuthEventPayloadSafe(t *testing.T, payload map[string]any) {
	t.Helper()
	for _, prohibited := range []string{"access_token", "refresh_token", "provider_response", "created_by_app_id"} {
		// Credential and internal routing fields must remain absent on every public lifecycle path.
		if _, leaked := payload[prohibited]; leaked {
			t.Fatalf("auth lifecycle payload leaked %q", prohibited)
		}
	}
}

// assertReconnectRequiredRedelivery NACKs one delivery and verifies JetStream preserves its stable event identity.
func assertReconnectRequiredRedelivery(t *testing.T, consumer *nats.Subscription, message *nats.Msg, eventID uuid.UUID) {
	t.Helper()
	// A failed handler disposition must preserve the same event for another attempt.
	if err := message.Nak(); err != nil {
		t.Fatalf("nack reconnect-required delivery: %v", err)
	}
	redelivered, err := consumer.Fetch(1, nats.MaxWait(5*time.Second))
	// JetStream redelivery proves the generated receiver can retry without a new auth transition.
	if err != nil || len(redelivered) != 1 {
		t.Fatalf("fetch redelivered reconnect webhook: messages=%d err=%v", len(redelivered), err)
	}
	if redelivered[0].Header.Get("X-Webhook-Msg-ID") != eventID.String() {
		t.Fatalf("redelivered message ID = %q, want %q", redelivered[0].Header.Get("X-Webhook-Msg-ID"), eventID)
	}
	_ = redelivered[0].Ack()
}

// waitForAuthEventProjectionAck waits until a bounded durable batch reaches terminal acknowledgement.
func waitForAuthEventProjectionAck(t *testing.T, js nats.JetStreamContext, expected uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	// Polling the durable avoids assuming scheduling order between publication and the worker goroutine.
	for time.Now().Before(deadline) {
		info, err := js.ConsumerInfo(messaging.FusedEngineStream, authEventWebhookConsumer)
		// Terminal ACK is observable only after the expected deliveries reached the durable consumer.
		if err == nil && info.NumAckPending == 0 && info.Delivered.Consumer >= expected {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("ineligible auth events did not reach terminal acknowledgement")
}

// ResolveAuthEventAppFamilies returns only requested identities, matching the PostgreSQL projection contract.
func (stub authEventResolverStub) ResolveAuthEventAppFamilies(_ context.Context, appIDs []uuid.UUID) (map[uuid.UUID]store.AuthEventAppFamily, error) {
	resolved := make(map[uuid.UUID]store.AuthEventAppFamily, len(appIDs))
	// The stub preserves batch membership so tests detect accidental broad family projection.
	for _, appID := range appIDs {
		identity, found := stub.identities[appID]
		// Unknown provenance stays absent so the worker can terminate it without family leakage.
		if found {
			resolved[appID] = identity
		}
	}
	return resolved, nil
}

// TestAuthEventWebhookWorkerProjectsThroughRealJetStream proves the internal-to-SDK delivery commit boundary and public payload shape.
func TestAuthEventWebhookWorkerProjectsThroughRealJetStream(t *testing.T) {
	js, natsClient := authEventWebhookTestJetStream(t)
	accountID := uuid.New()
	familyID := uuid.New()
	appID := uuid.New()
	serviceID := uuid.New()
	serviceVersionID := uuid.New()
	resolver := authEventResolverStub{identities: map[uuid.UUID]store.AuthEventAppFamily{
		appID: {AppID: appID, AppFamilyID: familyID, AccountID: accountID, Kind: store.AppKindSDK},
	}}
	worker, err := StartAuthEventWebhookWorker(context.Background(), resolver, natsClient)
	// Worker startup must succeed before the test publishes into its durable input.
	if err != nil {
		t.Fatalf("StartAuthEventWebhookWorker: %v", err)
	}
	// Cleanup drains the projector so its goroutine cannot outlive the broker fixture.
	t.Cleanup(func() { worker.Stop(context.Background()) })

	connection := store.AuthConnection{
		ID: uuid.New(), BucketID: uuid.New(), ServiceID: serviceID, ServiceVersionID: serviceVersionID,
		CreatedByAppID: appID, EndUserRef: "customer-42", AuthType: "oauth2", AuthName: "jira", RefreshState: "ok",
	}
	event := authevent.NewTokenRefreshed(connection, time.Now().UTC())
	// Canonical publication must succeed before public projection can be observed.
	if err := authevent.NewPublisher(natsClient).Publish(context.Background(), event); err != nil {
		t.Fatalf("publish internal auth event: %v", err)
	}

	subject := messaging.FusedAuthWebhookSubject(accountID, familyID, serviceID, "fused.auth.token.refreshed")
	consumer, err := js.PullSubscribe(subject, "auth-event-webhook-live-test", nats.BindStream("WEBHOOKS"))
	// The exact family/service subject prevents the test from passing through a broad stream fetch.
	if err != nil {
		t.Fatalf("subscribe projected webhook: %v", err)
	}
	messages, err := consumer.Fetch(1, nats.MaxWait(5*time.Second))
	// One projected input transition must produce exactly one retained public event.
	if err != nil || len(messages) != 1 {
		t.Fatalf("fetch projected webhook: messages=%d err=%v", len(messages), err)
	}
	message := messages[0]
	assertProjectedAuthWebhook(t, message, event, serviceID)
	_ = message.Ack()
}

// TestAuthEventWebhookWorkerRedeliversReconnectRequiredThroughRealJetStream proves a failed grant remains actionable and retryable.
func TestAuthEventWebhookWorkerRedeliversReconnectRequiredThroughRealJetStream(t *testing.T) {
	js, natsClient := authEventWebhookTestJetStream(t)
	accountID, familyID, appID := uuid.New(), uuid.New(), uuid.New()
	serviceID, serviceVersionID := uuid.New(), uuid.New()
	resolver := authEventResolverStub{identities: map[uuid.UUID]store.AuthEventAppFamily{
		appID: {AppID: appID, AppFamilyID: familyID, AccountID: accountID, Kind: store.AppKindSDK},
	}}
	worker, err := StartAuthEventWebhookWorker(context.Background(), resolver, natsClient)
	// Projection must be active before the rejected grant transition is published.
	if err != nil {
		t.Fatalf("StartAuthEventWebhookWorker: %v", err)
	}
	// Cleanup drains the worker before its isolated broker is stopped.
	t.Cleanup(func() { worker.Stop(context.Background()) })

	connection := store.AuthConnection{
		ID: uuid.New(), BucketID: uuid.New(), ServiceID: serviceID, ServiceVersionID: serviceVersionID,
		CreatedByAppID: appID, EndUserRef: "customer-42", AuthType: "oauth2", AuthName: "jira",
		RefreshState: "reconnect_required",
	}
	event := authevent.NewReconnectRequired(connection, "refresh_token_rejected", time.Now().UTC())
	// The canonical publisher must retain the failure decision before projection can ACK it.
	if err := authevent.NewPublisher(natsClient).Publish(context.Background(), event); err != nil {
		t.Fatalf("publish reconnect-required auth event: %v", err)
	}

	subject := messaging.FusedAuthWebhookSubject(accountID, familyID, serviceID, "fused.auth.connection.reconnect_required")
	consumer, err := js.PullSubscribe(subject, "auth-event-webhook-reconnect-test", nats.BindStream("WEBHOOKS"))
	// An exact subject prevents another family or service event from satisfying this assertion.
	if err != nil {
		t.Fatalf("subscribe projected reconnect webhook: %v", err)
	}
	first, err := consumer.Fetch(1, nats.MaxWait(5*time.Second))
	// One rejected grant must become one public lifecycle delivery.
	if err != nil || len(first) != 1 {
		t.Fatalf("fetch projected reconnect webhook: messages=%d err=%v", len(first), err)
	}
	assertProjectedAuthWebhook(t, first[0], event, serviceID)
	assertReconnectRequiredPayload(t, first[0].Data, "refresh_token_rejected")
	assertReconnectRequiredRedelivery(t, consumer, first[0], event.ID)
}

// TestAuthEventWebhookWorkerSkipsCLIAndMCPProvenance proves only SDK-attributed connections enter the generated receiver surface.
func TestAuthEventWebhookWorkerSkipsCLIAndMCPProvenance(t *testing.T) {
	js, natsClient := authEventWebhookTestJetStream(t)
	mcpAppID := uuid.New()
	resolver := authEventResolverStub{identities: map[uuid.UUID]store.AuthEventAppFamily{
		mcpAppID: {AppID: mcpAppID, AppFamilyID: uuid.New(), AccountID: uuid.New(), Kind: store.AppKindMCP},
	}}
	worker, err := StartAuthEventWebhookWorker(context.Background(), resolver, natsClient)
	// Worker startup must succeed before ineligible transitions are exercised.
	if err != nil {
		t.Fatalf("StartAuthEventWebhookWorker: %v", err)
	}
	// Cleanup drains the projector so its goroutine cannot outlive the broker fixture.
	t.Cleanup(func() { worker.Stop(context.Background()) })

	base := store.AuthConnection{
		ID: uuid.New(), BucketID: uuid.New(), ServiceID: uuid.New(), ServiceVersionID: uuid.New(),
		EndUserRef: "customer-42", AuthType: "oauth2", AuthName: "jira", RefreshState: "ok",
	}
	cliEvent := authevent.NewTokenRefreshed(base, time.Now().UTC())
	base.ID, base.CreatedByAppID = uuid.New(), mcpAppID
	mcpEvent := authevent.NewTokenRefreshed(base, time.Now().UTC())
	publisher := authevent.NewPublisher(natsClient)
	// CLI and MCP provenance exercise both ineligible attribution branches in one durable batch.
	for _, event := range []authevent.Event{cliEvent, mcpEvent} {
		// Both valid internal events remain auditable even though no SDK may receive them.
		if err := publisher.Publish(context.Background(), event); err != nil {
			t.Fatalf("publish internal auth event: %v", err)
		}
	}

	waitForAuthEventProjectionAck(t, js, 2)
	stream, err := js.StreamInfo("WEBHOOKS")
	// Output stream inspection must succeed to prove zero retained public events.
	if err != nil {
		t.Fatalf("read WEBHOOKS stream: %v", err)
	}
	// Ineligible provenance must not consume public webhook retention or receiver quota.
	if stream.State.Msgs != 0 {
		t.Fatalf("WEBHOOKS retained %d ineligible events", stream.State.Msgs)
	}
}

// authEventWebhookTestJetStream starts isolated real streams for projector integration tests.
func authEventWebhookTestJetStream(t *testing.T) (nats.JetStreamContext, *messaging.NATSClient) {
	t.Helper()
	natsServer, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	// Real broker construction is required to exercise retention and acknowledgement semantics.
	if err != nil {
		t.Fatalf("create NATS server: %v", err)
	}
	go natsServer.Start()
	// A bounded readiness wait prevents a broken broker fixture from hanging tests.
	if !natsServer.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server did not become ready")
	}
	t.Cleanup(natsServer.Shutdown)
	connection, err := nats.Connect(natsServer.ClientURL())
	// A network connection is needed so worker and publisher use normal JetStream APIs.
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(connection.Close)
	js, err := connection.JetStream()
	// JetStream initialization failure cannot fall back to core NATS for durability tests.
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	_, err = js.AddStream(&nats.StreamConfig{Name: messaging.FusedEngineStream, Subjects: messaging.FusedEngineStreamSubjects(), Storage: nats.MemoryStorage})
	// The internal stream retains canonical auth transitions before projection.
	if err != nil {
		t.Fatalf("add internal stream: %v", err)
	}
	_, err = js.AddStream(&nats.StreamConfig{Name: "WEBHOOKS", Subjects: []string{"webhooks.>"}, Storage: nats.MemoryStorage})
	// The public stream proves only authorized projected subjects are retained.
	if err != nil {
		t.Fatalf("add webhook stream: %v", err)
	}
	return js, &messaging.NATSClient{Conn: connection, JS: js}
}
