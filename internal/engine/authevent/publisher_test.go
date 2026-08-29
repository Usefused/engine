package authevent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// publisherCapture records the exact JetStream message without requiring a live broker.
type publisherCapture struct {
	message *nats.Msg
	err     error
}

// PublishMsgJS implements the narrow publication boundary used by Publisher.
func (capture *publisherCapture) PublishMsgJS(message *nats.Msg) (*nats.PubAck, error) {
	capture.message = message
	return &nats.PubAck{}, capture.err
}

// authEventFixture supplies complete non-secret routing identity for constructor and validation tests.
func authEventFixture() (store.ConnectSession, store.AuthConnection) {
	appID := uuid.New()
	connection := store.AuthConnection{
		ID: uuid.New(), BucketID: uuid.New(), ServiceID: uuid.New(), ServiceVersionID: uuid.New(),
		CreatedByAppID: appID, EndUserRef: "customer-42", AuthType: "OAuth2", AuthName: "oauth",
		RefreshState: "ok", EncryptedAccessToken: "ciphertext-that-must-not-publish",
	}
	session := store.ConnectSession{
		ID: uuid.New(), BucketID: connection.BucketID, ServiceID: connection.ServiceID,
		ServiceVersionID: connection.ServiceVersionID, CreatedByAppID: appID,
		EndUserRef: connection.EndUserRef, AuthType: connection.AuthType, AuthName: connection.AuthName,
	}
	return session, connection
}

// TestPublisherPublishesVersionedCredentialFreeConnectionEvent verifies the durable wire contract and de-duplication key.
func TestPublisherPublishesVersionedCredentialFreeConnectionEvent(t *testing.T) {
	session, connection := authEventFixture()
	occurredAt := time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC)
	capture := &publisherCapture{}

	event := NewConnectionCompleted(session, connection, 2, occurredAt)
	require.NoError(t, NewPublisher(capture).Publish(context.Background(), event))
	require.NotNil(t, capture.message)
	require.Equal(t, messaging.EngineAuthEventsSubject, capture.message.Subject)
	require.Equal(t, event.ID.String(), capture.message.Header.Get(nats.MsgIdHdr))

	var envelope Envelope
	require.NoError(t, json.Unmarshal(capture.message.Data, &envelope))
	require.Equal(t, SchemaVersion, envelope.SchemaVersion)
	require.Equal(t, TypeConnectionCompleted, envelope.Event.Type)
	require.Equal(t, connection.ID, envelope.Event.ConnectionID)
	require.Equal(t, session.ID, *envelope.Event.ConnectSessionID)
	require.Equal(t, 2, envelope.Event.ResourceCount)

	payload := string(capture.message.Data)
	// Provider credential field names and fixture ciphertext must remain absent even if the store model grows.
	for _, forbidden := range []string{"access_token", "refresh_token", "id_token", "encrypted", "ciphertext-that-must-not-publish"} {
		require.False(t, strings.Contains(payload, forbidden), "payload contains %q", forbidden)
	}
}

// TestPublisherAcceptsEveryCanonicalRefreshTransition locks the closed event vocabulary and state combinations.
func TestPublisherAcceptsEveryCanonicalRefreshTransition(t *testing.T) {
	_, connection := authEventFixture()
	occurredAt := time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC)
	events := []Event{
		NewTokenRefreshed(connection, occurredAt),
		NewTokenRefreshFailed(connection, "provider_refresh_failed", occurredAt),
		NewReconnectRequired(connection, "refresh_token_rejected", occurredAt),
	}

	// Every admitted refresh transition must serialize through the same publisher contract.
	for _, event := range events {
		event := event
		t.Run(string(event.Type), func(t *testing.T) {
			capture := &publisherCapture{}
			require.NoError(t, NewPublisher(capture).Publish(context.Background(), event))
			require.NotNil(t, capture.message)
		})
	}
}

// TestPublisherRejectsContradictoryAndUnsafeEvents prevents malformed producer state entering the shared stream.
func TestPublisherRejectsContradictoryAndUnsafeEvents(t *testing.T) {
	_, connection := authEventFixture()
	occurredAt := time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC)
	tests := map[string]Event{
		"missing selector": NewTokenRefreshed(store.AuthConnection{}, occurredAt),
		"unsafe code":      NewTokenRefreshFailed(connection, "provider said: token=secret", occurredAt),
		"callback identity": func() Event {
			event := NewTokenRefreshed(connection, occurredAt)
			event.ConnectSessionID = optionalUUID(uuid.New())
			return event
		}(),
	}

	// Independent invalid states remain named so failures identify the broken invariant.
	for name, event := range tests {
		t.Run(name, func(t *testing.T) {
			capture := &publisherCapture{}
			require.Error(t, NewPublisher(capture).Publish(context.Background(), event))
			require.Nil(t, capture.message)
		})
	}
}

// TestPublisherReturnsJetStreamFailure keeps post-commit producers able to observe broker outages.
func TestPublisherReturnsJetStreamFailure(t *testing.T) {
	_, connection := authEventFixture()
	publisher := NewPublisher(&publisherCapture{err: errors.New("unavailable")})

	err := publisher.Publish(context.Background(), NewTokenRefreshed(connection, time.Now().UTC()))
	require.ErrorContains(t, err, "publish auth event")
}

// TestPublisherTelemetryContainsOnlyBoundedDecisionFields prevents routing identity entering trace attributes.
func TestPublisherTelemetryContainsOnlyBoundedDecisionFields(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previousProvider) })
	_, connection := authEventFixture()
	ctx, span := otel.Tracer("test").Start(context.Background(), "auth-event-test")
	publisher := NewPublisher(&publisherCapture{})

	require.NoError(t, publisher.Publish(ctx, NewTokenRefreshFailed(connection, "provider_refresh_failed", time.Now().UTC())))
	unsafe := NewTokenRefreshFailed(connection, "provider said token=secret", time.Now().UTC())
	unsafe.Type = Type("provider-secret-event")
	require.Error(t, publisher.Publish(ctx, unsafe))
	span.End()

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	events := spans[0].Events()
	require.Len(t, events, 2)
	require.Equal(t, "engine.auth.event.publish", events[0].Name)
	attributes := events[0].Attributes
	require.Len(t, attributes, 3)
	// Only closed vocabulary fields are admitted; UUIDs, user references, and auth names stay on the internal event.
	for _, item := range attributes {
		require.Contains(t, []string{"auth.event.type", "auth.event.outcome", "auth.event.failure_code"}, string(item.Key))
	}
	invalidAttributes := events[1].Attributes
	require.Len(t, invalidAttributes, 2)
	// Malformed type and failure prose collapse instead of becoming trace values.
	require.Equal(t, "invalid", invalidAttributes[0].Value.AsString())
	require.Equal(t, "invalid", invalidAttributes[1].Value.AsString())
}

// TestDecodeRevalidatesDurableEnvelope proves projection consumers reject schema drift and unknown fields.
func TestDecodeRevalidatesDurableEnvelope(t *testing.T) {
	_, connection := authEventFixture()
	event := NewTokenRefreshed(connection, time.Now().UTC())
	payload, err := json.Marshal(Envelope{SchemaVersion: SchemaVersion, Event: event})
	require.NoError(t, err)

	decoded, err := Decode(payload)
	require.NoError(t, err)
	require.Equal(t, event.ID, decoded.ID)

	unknown := append(payload[:len(payload)-1], []byte(`,"future":true}`)...)
	// Unknown envelope fields require an explicit consumer contract update rather than silent public projection.
	_, err = Decode(unknown)
	require.Error(t, err)
	wrongVersion, err := json.Marshal(Envelope{SchemaVersion: SchemaVersion + 1, Event: event})
	require.NoError(t, err)
	_, err = Decode(wrongVersion)
	require.Error(t, err)
}

// TestWebhookEventNameUsesReservedNamespace locks the public names generated by SDK webhook handlers.
func TestWebhookEventNameUsesReservedNamespace(t *testing.T) {
	name, ok := WebhookEventName(TypeReconnectRequired)
	require.True(t, ok)
	require.Equal(t, "fused.auth.connection.reconnect_required", name)
	_, ok = WebhookEventName(Type("provider.detail"))
	// Unknown internal vocabulary must not be transformed into a seemingly trusted Fused event name.
	require.False(t, ok)
}

// TestIsWebhookEventNameKeepsProjectionAndAdmissionInParity covers every closed lifecycle type plus near misses.
func TestIsWebhookEventNameKeepsProjectionAndAdmissionInParity(t *testing.T) {
	// Every canonical internal transition must round-trip through its public SDK name.
	for _, eventType := range []Type{TypeConnectionCompleted, TypeTokenRefreshed, TypeTokenRefreshFailed, TypeReconnectRequired} {
		name, ok := WebhookEventName(eventType)
		// Every name emitted by the projector must be admitted by webhook subscription validation.
		require.True(t, ok)
		require.True(t, IsWebhookEventName(name), name)
	}
	// Near misses prove namespace containment and unknown vocabulary remain rejected.
	for _, name := range []string{"connection.completed", "provider.fused.auth.connection.completed", "fused.auth.unknown"} {
		// Namespace lookalikes remain provider data rather than gaining reserved routing privileges.
		require.False(t, IsWebhookEventName(name), name)
	}
}
