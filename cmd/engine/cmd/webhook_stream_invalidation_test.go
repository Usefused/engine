package cmd

import (
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/webhookstream"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startCoreNATSTestClient creates one real core-NATS connection for peer invalidation coverage.
func startCoreNATSTestClient(t *testing.T) (*messaging.NATSClient, func()) {
	t.Helper()
	server, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1})
	// Broker construction failures make the peer-path integration test unusable.
	if err != nil {
		t.Fatalf("create NATS server: %v", err)
	}
	go server.Start()
	// A bounded readiness wait prevents a broken embedded broker from hanging the suite.
	if !server.ReadyForConnections(5 * time.Second) {
		server.Shutdown()
		t.Fatal("NATS server did not become ready")
	}
	connection, err := nats.Connect(server.ClientURL())
	// A real client is required to prove subscription and peer publication ordering.
	if err != nil {
		server.Shutdown()
		t.Fatalf("connect NATS: %v", err)
	}
	// Cleanup closes the client before the broker so no reconnect attempt can outlive the fixture.
	cleanup := func() {
		connection.Close()
		server.Shutdown()
	}
	return &messaging.NATSClient{Conn: connection}, cleanup
}

// mustRegisterWebhookStream creates one confirmed exact-app fixture for peer invalidation assertions.
func mustRegisterWebhookStream(t *testing.T, registry *webhookstream.Registry, tokenID, appID uuid.UUID) *webhookstream.Registration {
	t.Helper()
	registration, ok := registry.Register(tokenID, appID)
	// Fixtures represent receivers that already passed their authoritative source recheck.
	if !ok || !registration.Confirm() {
		t.Fatal("confirm webhook stream registration")
	}
	return registration
}

// publishSDKScopeInvalidation sends one exact-app peer signal after establishing subscriber interest.
func publishSDKScopeInvalidation(t *testing.T, client *messaging.NATSClient, appID uuid.UUID) {
	t.Helper()
	// Flush establishes the subscription before the simulated peer publishes its immutable runtime change.
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}
	if err := client.Conn.Publish("engine.cache.invalidate.sdk_scope."+appID.String(), nil); err != nil {
		t.Fatalf("publish peer invalidation: %v", err)
	}
	// A second flush bounds callback delivery before the test inspects cancellation state.
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush peer invalidation: %v", err)
	}
}

// TestSDKScopePeerInvalidationClosesOnlyExactAppStreams exercises the production core-NATS subscriber.
func TestSDKScopePeerInvalidationClosesOnlyExactAppStreams(t *testing.T) {
	client, cleanup := startCoreNATSTestClient(t)
	defer cleanup()
	registry := webhookstream.NewRegistry()
	invalidatedAppID, siblingAppID, tokenID := uuid.New(), uuid.New(), uuid.New()
	invalidated := mustRegisterWebhookStream(t, registry, tokenID, invalidatedAppID)
	sibling := mustRegisterWebhookStream(t, registry, tokenID, siblingAppID)
	defer sibling.Unregister()

	subscribeCacheInvalidation(client, nil, registry)
	publishSDKScopeInvalidation(t, client, invalidatedAppID)

	select {
	case <-invalidated.Done():
		// Exact target cancellation proves the core-NATS path reaches the live registry.
	case <-time.After(time.Second):
		t.Fatal("peer invalidation did not close exact-app stream")
	}
	select {
	case <-sibling.Done():
		// Sibling versions use different durables and authorization registrations.
		t.Fatal("peer invalidation closed sibling exact-app stream")
	default:
		// Immediate liveness is sufficient because target cancellation already proves callback completion.
	}
}
