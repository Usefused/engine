package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/authevent"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/google/uuid"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// TestMCPAuthCorrelationRequiresExactAppAndDeliversOnce locks ownership and idempotence before the NATS boundary.
func TestMCPAuthCorrelationRequiresExactAppAndDeliversOnce(t *testing.T) {
	resetMCPAuthCorrelationTestState(t)
	session, appID := installMCPAuthCorrelationTestSession(t)
	connectSessionID := uuid.New()
	expiresAt := time.Now().UTC().Add(time.Minute)
	// The browser URL cannot be returned unless its persisted UUID is already mapped to this capable live session.
	if !registerMCPAuthCorrelation(context.Background(), session, connectSessionID.String(), expiresAt.Format(time.RFC3339)) {
		t.Fatal("registerMCPAuthCorrelation rejected a valid live session")
	}
	event := mcpAuthCompletionTestEvent(connectSessionID, appID)
	wrongApp := event
	wrongApp.CreatedByAppID = optionalMCPAuthTestUUID(uuid.New())
	// A misattributed durable event cannot consume the later correctly attributed completion.
	if outcome := pendingMCPAuthCorrelations.complete(wrongApp, time.Now().UTC()); outcome != "identity_mismatch" {
		t.Fatalf("misattributed completion outcome = %q", outcome)
	}
	if outcome := pendingMCPAuthCorrelations.complete(event, time.Now().UTC()); outcome != "delivered" {
		t.Fatalf("valid completion outcome = %q", outcome)
	}
	payload := <-session.serverNotifications
	// The server notification contains only the standard method and opaque UUID correlation.
	if !strings.Contains(payload, `"method":"notifications/elicitation/complete"`) || !strings.Contains(payload, connectSessionID.String()) {
		t.Fatalf("completion notification = %s", payload)
	}
	// Redelivery or a second replica observation cannot enqueue a duplicate notification.
	if outcome := pendingMCPAuthCorrelations.complete(event, time.Now().UTC()); outcome != "unmatched" {
		t.Fatalf("duplicate completion outcome = %q", outcome)
	}
}

// TestMCPAuthCorrelationBoundsAndPrunesPerSession proves pending work cannot grow with repeated unresolved browser prompts.
func TestMCPAuthCorrelationBoundsAndPrunesPerSession(t *testing.T) {
	registry := mcpAuthCorrelationRegistry{
		byConnectSession: make(map[uuid.UUID]pendingMCPAuthCorrelation),
		byMCPSession:     make(map[string]map[uuid.UUID]time.Time),
	}
	session := &mcpSession{sessionID: uuid.NewString()}
	appID := uuid.New()
	now := time.Now().UTC()
	firstID := uuid.New()
	// The first short-lived action gives the later admission a deterministic expired entry to prune.
	if outcome := registry.register(session, appID, firstID, now.Add(time.Second), now); outcome != "registered" {
		t.Fatalf("first registration outcome = %q", outcome)
	}
	for index := 1; index < maxPendingMCPAuthCorrelations; index++ {
		if outcome := registry.register(session, appID, uuid.New(), now.Add(time.Minute), now); outcome != "registered" {
			t.Fatalf("registration %d outcome = %q", index, outcome)
		}
	}
	// Capacity rejection occurs before another URL can be exposed to the client.
	if outcome := registry.register(session, appID, uuid.New(), now.Add(time.Minute), now); outcome != "capacity" {
		t.Fatalf("over-capacity outcome = %q", outcome)
	}
	// Once the advertised expiry passes, admission prunes that exact action and reuses its bounded slot.
	if outcome := registry.register(session, appID, uuid.New(), now.Add(2*time.Minute), now.Add(2*time.Second)); outcome != "registered" {
		t.Fatalf("post-prune outcome = %q", outcome)
	}
	if _, exists := registry.byConnectSession[firstID]; exists {
		t.Fatal("expired correlation survived bounded pruning")
	}
}

// TestMCPAuthCorrelationCannotRegisterAfterLifecycleRemoval covers the termination race at the shared lock boundary.
func TestMCPAuthCorrelationCannotRegisterAfterLifecycleRemoval(t *testing.T) {
	resetMCPAuthCorrelationTestState(t)
	session, _ := installMCPAuthCorrelationTestSession(t)
	connectSessionID := uuid.NewString()
	result := make(chan bool, 1)
	session.lifecycleMu.Lock()
	go func() {
		result <- registerMCPAuthCorrelation(context.Background(), session, connectSessionID, time.Now().UTC().Add(time.Minute).Format(time.RFC3339))
	}()
	// Removal while lifecycle ownership is held mirrors termination's claim before its deferred correlation cleanup.
	mcpSessions.Lock()
	delete(mcpSessions.m, session.sessionID)
	mcpSessions.Unlock()
	session.lifecycleMu.Unlock()
	// The delayed registration must observe removal rather than recreating stale reverse state after cleanup.
	if <-result {
		t.Fatal("correlation registered after session removal")
	}
	pendingMCPAuthCorrelations.Lock()
	defer pendingMCPAuthCorrelations.Unlock()
	if len(pendingMCPAuthCorrelations.byConnectSession) != 0 || len(pendingMCPAuthCorrelations.byMCPSession) != 0 {
		t.Fatalf("stale correlation state = connect:%d session:%d", len(pendingMCPAuthCorrelations.byConnectSession), len(pendingMCPAuthCorrelations.byMCPSession))
	}
}

// TestMCPAuthCompletionFlowsThroughNATS verifies a committed lifecycle event reaches the standard Streamable notification on its owning replica.
func TestMCPAuthCompletionFlowsThroughNATS(t *testing.T) {
	resetMCPAuthCorrelationTestState(t)
	natsServer, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("create NATS server: %v", err)
	}
	go natsServer.Start()
	if !natsServer.ReadyForConnections(10 * time.Second) {
		t.Fatal("NATS server did not become ready")
	}
	t.Cleanup(natsServer.Shutdown)
	connection, err := nats.Connect(natsServer.ClientURL())
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(connection.Close)
	jetStream, err := connection.JetStream()
	if err != nil {
		t.Fatalf("create JetStream context: %v", err)
	}
	if _, err := jetStream.AddStream(&nats.StreamConfig{Name: messaging.FusedEngineStream, Subjects: messaging.FusedEngineStreamSubjects(), Storage: nats.MemoryStorage}); err != nil {
		t.Fatalf("create Engine stream: %v", err)
	}
	client := &messaging.NATSClient{Conn: connection, JS: jetStream}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := StartMCPAuthCompletionSubscriber(ctx, client); err != nil {
		t.Fatalf("start completion subscriber: %v", err)
	}
	session, appID := installMCPAuthCorrelationTestSession(t)
	connectSessionID := uuid.New()
	if !registerMCPAuthCorrelation(context.Background(), session, connectSessionID.String(), time.Now().UTC().Add(time.Minute).Format(time.RFC3339)) {
		t.Fatal("register NATS correlation")
	}
	event := mcpAuthCompletionTestEvent(connectSessionID, appID)
	if err := authevent.NewPublisher(client).Publish(context.Background(), event); err != nil {
		t.Fatalf("publish connection completion: %v", err)
	}
	select {
	case payload := <-session.serverNotifications:
		var notification struct {
			Method string `json:"method"`
			Params struct {
				ElicitationID string `json:"elicitationId"`
			} `json:"params"`
		}
		// Real broker delivery must retain the standard notification shape and the persisted UUID exactly.
		if json.Unmarshal([]byte(payload), &notification) != nil || notification.Method != "notifications/elicitation/complete" || notification.Params.ElicitationID != connectSessionID.String() {
			t.Fatalf("NATS completion notification = %s", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for MCP auth completion")
	}
}

// installMCPAuthCorrelationTestSession installs one capable process-local session without starting a child runtime.
func installMCPAuthCorrelationTestSession(t *testing.T) (*mcpSession, uuid.UUID) {
	t.Helper()
	appID := uuid.New()
	session := &mcpSession{
		appID: appID.String(), sessionID: uuid.NewString(), transport: mcpStreamableTransport, urlElicitation: true,
		serverNotifications: make(chan string, maxMCPServerNotifications),
		authActions:         make(chan struct{}, maxPendingMCPAuthCorrelations),
	}
	mcpSessions.Lock()
	mcpSessions.m[session.sessionID] = session
	mcpSessions.Unlock()
	t.Cleanup(func() {
		mcpSessions.Lock()
		delete(mcpSessions.m, session.sessionID)
		mcpSessions.Unlock()
		unregisterMCPAuthCorrelations(session)
	})
	return session, appID
}

// mcpAuthCompletionTestEvent builds a valid credential-free completion with exact app and connect-session identity.
func mcpAuthCompletionTestEvent(connectSessionID, appID uuid.UUID) authevent.Event {
	now := time.Now().UTC()
	return authevent.NewConnectionCompleted(store.ConnectSession{ID: connectSessionID}, store.AuthConnection{
		ID: uuid.New(), BucketID: uuid.New(), ServiceID: uuid.New(), ServiceVersionID: uuid.New(), CreatedByAppID: appID,
		EndUserRef: "user-test", AuthType: "oauth", AuthName: "OAuth2", RefreshState: "ok",
	}, 0, now)
}

// optionalMCPAuthTestUUID returns a stable pointer for deliberate provenance mutation in tests.
func optionalMCPAuthTestUUID(value uuid.UUID) *uuid.UUID {
	return &value
}

// resetMCPAuthCorrelationTestState isolates process-global routing maps between focused tests.
func resetMCPAuthCorrelationTestState(t *testing.T) {
	t.Helper()
	pendingMCPAuthCorrelations.Lock()
	pendingMCPAuthCorrelations.byConnectSession = make(map[uuid.UUID]pendingMCPAuthCorrelation)
	pendingMCPAuthCorrelations.byMCPSession = make(map[string]map[uuid.UUID]time.Time)
	pendingMCPAuthCorrelations.Unlock()
	t.Cleanup(func() {
		pendingMCPAuthCorrelations.Lock()
		pendingMCPAuthCorrelations.byConnectSession = make(map[uuid.UUID]pendingMCPAuthCorrelation)
		pendingMCPAuthCorrelations.byMCPSession = make(map[string]map[uuid.UUID]time.Time)
		pendingMCPAuthCorrelations.Unlock()
	})
}
