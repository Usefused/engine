package bridgehost

import (
	"context"
	"encoding/json"
	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"sync/atomic"
	"testing"
	"time"
)

// brokerFixture uses an isolated file store and loopback port for real broker acknowledgement tests.
func brokerFixture(t *testing.T) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	server, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true, MaxPayload: 4 << 20})
	require.NoError(t, err)
	go server.Start()
	require.True(t, server.ReadyForConnections(5*time.Second))
	t.Cleanup(server.Shutdown)
	conn, err := nats.Connect(server.ClientURL())
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	require.NoError(t, err)
	_, err = js.AddStream(&nats.StreamConfig{Name: "WEBHOOKS", Subjects: []string{"webhooks.>"}, Storage: nats.FileStorage})
	require.NoError(t, err)
	return conn, js
}

// TestBridgeOutboxFanoutAndReplay proves accepted payloads reach independent durable consumers before database cleanup.
func TestBridgeOutboxFanoutAndReplay(t *testing.T) {
	repo, actor := databaseFixture(t)
	p, _ := pairingFixture(t, repo, actor)
	manifest := eventManifest(t, "1", false)
	require.NoError(t, repo.PutManifest(t.Context(), p, manifest))
	var document ManifestDocument
	require.NoError(t, json.Unmarshal(manifest.ManifestJson, &document))
	event := &bridgev1.Event{EventId: uuid.NewString(), ProfileId: "sensors", ConnectionId: "mqtt", OperationId: "messages", ManifestHash: manifest.Hash, ContractHash: document.Operations[0].ContractHash, PayloadJson: []byte(`42`), CapturedUnixMs: time.Now().UnixMilli(), DestinationId: p.DestinationID}
	require.NoError(t, repo.AcceptEvent(t.Context(), p, event))
	conn, js := brokerFixture(t)
	pin := Pin{PairingID: p.ID, ProfileID: event.ProfileId, ManifestHash: event.ManifestHash, ConnectionID: event.ConnectionId, OperationID: event.OperationId, ContractHash: event.ContractHash}
	first, err := js.PullSubscribe(EventSubject(p.AccountID, pin), "first", nats.ManualAck())
	require.NoError(t, err)
	second, err := js.PullSubscribe(EventSubject(p.AccountID, pin), "second", nats.ManualAck())
	require.NoError(t, err)
	// Removed profiles retain accepted payloads without delivering them to consumers.
	require.NoError(t, repo.PutInventory(t.Context(), p, inventoryFixture(t)))
	moved, err := repo.DeliverEvent(t.Context(), js)
	require.NoError(t, err)
	require.False(t, moved)
	// Restoring an offline profile permits its already accepted historical backlog to drain.
	require.NoError(t, repo.PutInventory(t.Context(), p, inventoryFixture(t, "sensors")))
	moved, err = repo.DeliverEvent(t.Context(), js)
	require.NoError(t, err)
	require.True(t, moved)
	for _, consumer := range []*nats.Subscription{first, second} {
		messages, err := consumer.Fetch(1, nats.MaxWait(time.Second))
		require.NoError(t, err)
		require.Equal(t, `42`, string(messages[0].Data))
		require.Equal(t, event.EventId, messages[0].Header.Get("X-Bridge-Event-ID"))
		require.NoError(t, messages[0].AckSync())
	}
	var bytes int
	var delivered bool
	require.NoError(t, repo.Pool.QueryRow(t.Context(), `SELECT octet_length(envelope),delivered_at IS NOT NULL FROM fused_bridge_events WHERE pairing_id=$1`, p.ID).Scan(&bytes, &delivered))
	require.Zero(t, bytes)
	require.True(t, delivered)
	// A retry after cleanup hits the durable content tombstone instead of republishing an event.
	require.NoError(t, repo.AcceptEvent(t.Context(), p, event))
	moved, err = repo.DeliverEvent(t.Context(), js)
	require.NoError(t, err)
	require.False(t, moved)
	event.EventId = uuid.NewString()
	require.NoError(t, repo.AcceptEvent(t.Context(), p, event))
	conn.Close()
	// Broker failure must retain the complete accepted envelope for a later handoff.
	moved, err = repo.DeliverEvent(t.Context(), js)
	require.Error(t, err)
	require.False(t, moved)
	require.NoError(t, repo.Pool.QueryRow(t.Context(), `SELECT octet_length(envelope) FROM fused_bridge_events WHERE pairing_id=$1 AND event_id=$2`, p.ID, event.EventId).Scan(&bytes))
	require.Positive(t, bytes)
}

// TestBridgeReplicaRoutingNeverRetriesUnknownCommands exercises real internal NATS routing and durable generation ownership.
func TestBridgeReplicaRoutingNeverRetriesUnknownCommands(t *testing.T) {
	repo, actor := databaseFixture(t)
	p, _ := pairingFixture(t, repo, actor)
	conn, _ := brokerFixture(t)
	owner, caller := New(repo.Pool, conn), New(repo.Pool, conn)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, owner.StartRouting(ctx))
	require.NoError(t, caller.StartRouting(ctx))
	var err error
	p.Generation, err = repo.Claim(ctx, p, owner.instance)
	require.NoError(t, err)
	require.NoError(t, repo.PutInventory(ctx, p, inventoryFixture(t, "sensors")))
	contract := `{"kind":"command","input_schema":{"type":"integer"},"output_schema":{"type":"integer"}}`
	doc := ManifestDocument{Version: 1, ProfileID: "sensors", Revision: "1", Operations: []Operation{{ConnectionID: "device", OperationID: "set", ContractHash: Digest([]byte(contract)), ContractJSON: contract}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	manifest := &bridgev1.Manifest{ProfileId: doc.ProfileID, Revision: doc.Revision, Hash: Digest(raw), ManifestJson: raw}
	require.NoError(t, repo.PutManifest(ctx, p, manifest))
	current := &session{pairing: p, out: make(chan *bridgev1.EngineFrame, 8), results: map[string]chan *bridgev1.CallResult{}, done: make(chan struct{})}
	owner.sessions[p.ID] = current
	var calls atomic.Int32
	// This peer acknowledges one command and deliberately loses the second reply after dispatch.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case frame := <-current.out:
				call := frame.GetCall()
				// Cancellation frames carry no new provider work.
				if call == nil {
					continue
				}
				// A lost reply simulates the exact uncertainty that must suppress retries.
				if calls.Add(1) != 1 {
					continue
				}
				current.mu.Lock()
				result := current.results[call.RequestId]
				current.mu.Unlock()
				result <- &bridgev1.CallResult{RequestId: call.RequestId, Generation: call.Generation, Outcome: "completed", ResultJson: call.InputJson}
			}
		}
	}()
	pin := Pin{PairingID: p.ID, ProfileID: doc.ProfileID, ManifestHash: manifest.Hash, ConnectionID: "device", OperationID: "set", ContractHash: doc.Operations[0].ContractHash}
	result, err := caller.Invoke(ctx, p.AccountID, pin, []byte(`20`))
	require.NoError(t, err)
	require.Equal(t, `20`, string(result))
	// Stopping a profile retains its schema but cannot send a second provider command.
	offline := doc
	offline.State = "offline"
	offline.Revision = "2"
	offline.Operations = []Operation{}
	offlineRaw, err := json.Marshal(offline)
	require.NoError(t, err)
	require.NoError(t, repo.PutManifest(ctx, p, &bridgev1.Manifest{ProfileId: offline.ProfileID, Revision: offline.Revision, Hash: Digest(offlineRaw), ManifestJson: offlineRaw}))
	_, err = caller.Invoke(ctx, p.AccountID, pin, []byte(`21`))
	require.Error(t, err)
	require.Equal(t, int32(1), calls.Load())
	// A fresh online declaration restores the same explicitly pinned capability.
	require.NoError(t, repo.PutManifest(ctx, p, manifest))
	deadline, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	_, err = caller.Invoke(deadline, p.AccountID, pin, []byte(`21`))
	var failure *InvocationError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "unknown", failure.Outcome)
	require.Equal(t, int32(2), calls.Load())
}
