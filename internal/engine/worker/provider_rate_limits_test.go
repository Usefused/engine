package worker

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func TestProviderRateLimitProjectionWorkerConsumesKVRevision(t *testing.T) {
	client := projectionTestNATSClient(t)
	kv, err := client.InitProviderRateLimitBucket()
	if err != nil {
		t.Fatal(err)
	}
	projection := &captureRateLimitProjection{persisted: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	worker, err := StartProviderRateLimitProjectionWorker(ctx, projection, client)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		worker.Stop(stopCtx)
	})

	state := projectionWorkerState()
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Put("v1.test", payload); err != nil {
		t.Fatal(err)
	}
	select {
	case <-projection.persisted:
	case <-time.After(5 * time.Second):
		t.Fatal("projection worker did not persist KV revision")
	}
	projection.mu.Lock()
	defer projection.mu.Unlock()
	if len(projection.states) != 1 || projection.states[0].AccountID != state.AccountID {
		t.Fatalf("persisted states = %#v", projection.states)
	}
}

func TestDecodeProviderRateLimitMessagesRejectsInvalidSchema(t *testing.T) {
	valid, _ := json.Marshal(projectionWorkerState())
	invalidState := projectionWorkerState()
	invalidState.SchemaVersion++
	invalid, _ := json.Marshal(invalidState)
	states, messages := decodeProviderRateLimitMessages([]*nats.Msg{{Data: valid}, {Data: invalid}})
	if len(states) != 1 || len(messages) != 1 {
		t.Fatalf("states=%d messages=%d, want 1/1", len(states), len(messages))
	}
}

type captureRateLimitProjection struct {
	mu        sync.Mutex
	states    []ratelimitpolicy.StateEnvelope
	persisted chan struct{}
}

func (c *captureRateLimitProjection) BatchUpsertProviderRateLimitStates(_ context.Context, states []ratelimitpolicy.StateEnvelope) error {
	c.mu.Lock()
	c.states = append(c.states, states...)
	c.mu.Unlock()
	select {
	case c.persisted <- struct{}{}:
	default:
	}
	return nil
}

func projectionWorkerState() ratelimitpolicy.StateEnvelope {
	now := time.Now().UTC()
	return ratelimitpolicy.StateEnvelope{
		SchemaVersion: ratelimitpolicy.ProviderRateLimitStateSchemaVersion,
		AccountID:     uuid.New(), ServiceVersionID: uuid.New(), UpdatedAt: now,
		Policies: []ratelimitpolicy.PolicyState{{
			Name: "primary", ScopeKind: "connection", ScopeID: uuid.New(),
			ConfigHash: "config", Algorithm: "fixed_window",
			FixedWindowStartedAt: &now,
		}},
	}
}

func projectionTestNATSClient(t *testing.T) *messaging.NATSClient {
	t.Helper()
	natsServer, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	go natsServer.Start()
	if !natsServer.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server did not become ready")
	}
	t.Cleanup(natsServer.Shutdown)
	connection, err := nats.Connect(natsServer.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(connection.Close)
	jetStream, err := connection.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	return &messaging.NATSClient{Conn: connection, JS: jetStream}
}
