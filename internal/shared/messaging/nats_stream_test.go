package messaging

import (
	"testing"
	"time"

	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// TestInitStreamPersistsWebhookEventsForThirtyDays verifies the broker policy used by webhook ingress and SDK replay.
func TestInitStreamPersistsWebhookEventsForThirtyDays(t *testing.T) {
	natsServer, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	// Stream policy cannot be verified without an isolated file-backed JetStream server.
	if err != nil {
		t.Fatalf("create NATS server: %v", err)
	}
	go natsServer.Start()
	// Test traffic must wait until the embedded broker is accepting client connections.
	if !natsServer.ReadyForConnections(10 * time.Second) {
		t.Fatal("NATS server did not become ready")
	}
	t.Cleanup(natsServer.Shutdown)
	connection, err := nats.Connect(natsServer.ClientURL())
	// A real JetStream context is required to inspect the applied storage contract.
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(connection.Close)
	jetStream, err := connection.JetStream()
	// Failure to enable JetStream invalidates the retention assertion rather than skipping it.
	if err != nil {
		t.Fatalf("create JetStream context: %v", err)
	}
	client := &NATSClient{Conn: connection, JS: jetStream}
	if err := client.InitStream("WEBHOOKS", []string{"webhooks.>"}); err != nil {
		t.Fatalf("initialize webhook stream: %v", err)
	}
	if _, err := client.PublishJS("webhooks.account.service.attachment.payment.succeeded", []byte(`{"type":"payment.succeeded"}`)); err != nil {
		t.Fatalf("publish retained webhook: %v", err)
	}
	info, err := jetStream.StreamInfo("WEBHOOKS")
	// The acknowledged event and effective stream configuration must be observable together.
	if err != nil {
		t.Fatalf("read webhook stream: %v", err)
	}
	// Webhook ingress is file-backed for the configured age; consumer ACKs advance delivery state independently.
	if info.Config.Storage != nats.FileStorage || info.Config.MaxAge != 30*24*time.Hour || info.State.Msgs != 1 {
		t.Fatalf("storage=%v max_age=%v messages=%d", info.Config.Storage, info.Config.MaxAge, info.State.Msgs)
	}
}
