package authevent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/messaging"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// TestPublisherDeliversLifecycleEventsThroughJetStream proves every versioned auth transition survives real stream storage and consumption.
func TestPublisherDeliversLifecycleEventsThroughJetStream(t *testing.T) {
	natsServer, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(),
	})
	require.NoError(t, err)
	go natsServer.Start()
	require.True(t, natsServer.ReadyForConnections(5*time.Second), "NATS server did not become ready")
	t.Cleanup(natsServer.Shutdown)

	connection, err := nats.Connect(natsServer.ClientURL())
	require.NoError(t, err)
	t.Cleanup(connection.Close)
	jetStream, err := connection.JetStream()
	require.NoError(t, err)
	_, err = jetStream.AddStream(&nats.StreamConfig{
		Name: messaging.FusedEngineStream, Subjects: messaging.FusedEngineStreamSubjects(), Storage: nats.MemoryStorage,
	})
	require.NoError(t, err)
	consumer, err := jetStream.PullSubscribe(messaging.EngineAuthEventsSubject, "auth-event-live-test")
	require.NoError(t, err)

	session, authConnection := authEventFixture()
	now := time.Now().UTC()
	events := []Event{
		NewConnectionCompleted(session, authConnection, 2, now),
		NewTokenRefreshed(authConnection, now.Add(time.Second)),
		NewTokenRefreshFailed(authConnection, "provider_refresh_failed", now.Add(2*time.Second)),
		NewReconnectRequired(authConnection, "refresh_token_rejected", now.Add(3*time.Second)),
	}
	publisher := NewPublisher(&messaging.NATSClient{Conn: connection, JS: jetStream})
	// Publication order is retained so one connection's lifecycle can be consumed deterministically.
	for _, event := range events {
		require.NoError(t, publisher.Publish(context.Background(), event))
	}
	messages, err := consumer.Fetch(len(events), nats.MaxWait(2*time.Second))
	require.NoError(t, err)
	require.Len(t, messages, len(events))

	// Each consumed document must retain its exact event identity and credential-free routing projection.
	for index, message := range messages {
		var envelope Envelope
		require.NoError(t, json.Unmarshal(message.Data, &envelope))
		require.Equal(t, SchemaVersion, envelope.SchemaVersion)
		require.Equal(t, events[index].ID, envelope.Event.ID)
		require.Equal(t, events[index].Type, envelope.Event.Type)
		require.Equal(t, authConnection.ID, envelope.Event.ConnectionID)
		require.NoError(t, message.AckSync())
	}

	streamInfo, err := jetStream.StreamInfo(messaging.FusedEngineStream)
	require.NoError(t, err)
	require.EqualValues(t, len(events), streamInfo.State.Msgs)
}
