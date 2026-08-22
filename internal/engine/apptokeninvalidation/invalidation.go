// Package apptokeninvalidation propagates precise runtime-token revocations
// between Engine processes without putting credential material on NATS.
package apptokeninvalidation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var ErrPublisherUnavailable = errors.New("app-token invalidation publisher unavailable")

type outcome string

const (
	outcomeInvalid              outcome = "invalid"
	outcomeUnavailable          outcome = "unavailable"
	outcomeMarshalFailed        outcome = "marshal_failed"
	outcomePublishFailed        outcome = "publish_failed"
	outcomePublished            outcome = "published"
	outcomePersistenceFailed    outcome = "persistence_failed"
	outcomeRevokedPublishFailed outcome = "revoked_publish_failed"
	outcomeRevokedPublished     outcome = "revoked_published"
	outcomeInvalidated          outcome = "invalidated"
)

type repository interface {
	RevokeAppToken(context.Context, uuid.UUID, string) (*store.AppTokenRevocation, error)
}

// Invalidator is intentionally narrower than the runtime validator. It lets
// both the revoke path and subscriber evict one opaque token identity without
// gaining access to plaintext or hashes.
type Invalidator interface {
	InvalidateToken(uuid.UUID) int
}

type jetStreamPublisher interface {
	PublishMsgJS(*nats.Msg) (*nats.PubAck, error)
}

type jetStreamSubscriber interface {
	Subscribe(string, nats.MsgHandler, ...nats.SubOpt) (*nats.Subscription, error)
}

// Event is versioned by its NATS subject. These are the only fields allowed on
// the durable wire contract; labels and credential material stay in Postgres.
type Event struct {
	EventID     uuid.UUID `json:"event_id"`
	TokenID     uuid.UUID `json:"token_id"`
	AppFamilyID uuid.UUID `json:"app_family_id"`
	RevokedAt   time.Time `json:"revoked_at"`
}

type Publisher struct {
	js jetStreamPublisher
}

func NewPublisher(js jetStreamPublisher) *Publisher {
	return &Publisher{js: js}
}

func (p *Publisher) Publish(ctx context.Context, revocation store.AppTokenRevocation) error {
	event := Event{
		EventID: uuid.New(), TokenID: revocation.TokenID,
		AppFamilyID: revocation.AppFamilyID, RevokedAt: revocation.RevokedAt.UTC(),
	}
	if err := validateEvent(event); err != nil {
		recordPublish(ctx, event.AppFamilyID, outcomeInvalid)
		return err
	}
	if p == nil || p.js == nil {
		recordPublish(ctx, event.AppFamilyID, outcomeUnavailable)
		return ErrPublisherUnavailable
	}
	payload, err := json.Marshal(event)
	if err != nil {
		recordPublish(ctx, event.AppFamilyID, outcomeMarshalFailed)
		return fmt.Errorf("marshal app-token invalidation: %w", err)
	}
	message := nats.NewMsg(messaging.AppTokenInvalidatedSubject)
	message.Data = payload
	// JetStream de-duplicates a retry without using token identity as the
	// message ID or exposing credential-derived values in headers.
	message.Header.Set(nats.MsgIdHdr, event.EventID.String())
	if _, err := p.js.PublishMsgJS(message); err != nil {
		recordPublish(ctx, event.AppFamilyID, outcomePublishFailed)
		return fmt.Errorf("publish app-token invalidation: %w", err)
	}
	recordPublish(ctx, event.AppFamilyID, outcomePublished)
	return nil
}

// Service owns the post-commit order: evict this process first, then publish
// for peers. Publication failure cannot resurrect a token already deleted in
// PostgreSQL; the bounded cache TTL remains the recovery path.
type Service struct {
	repository  repository
	invalidator Invalidator
	publisher   *Publisher
}

func NewService(repository repository, invalidator Invalidator, publisher *Publisher) (*Service, error) {
	if repository == nil || invalidator == nil || publisher == nil || publisher.js == nil {
		return nil, errors.New("app-token invalidation service dependencies are required")
	}
	return &Service{repository: repository, invalidator: invalidator, publisher: publisher}, nil
}

func (s *Service) RevokeAppToken(ctx context.Context, appFamilyID uuid.UUID, name string) (*store.AppTokenRevocation, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.app_token.revoke.propagate")
	defer span.End()
	span.SetAttributes(attribute.String("app.family_id", appFamilyID.String()))
	revocation, err := s.repository.RevokeAppToken(ctx, appFamilyID, name)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", string(outcomePersistenceFailed)))
		span.SetStatus(codes.Error, "app-token revocation persistence failed")
		return nil, err
	}
	invalidated := s.invalidator.InvalidateToken(revocation.TokenID)
	// The fanout can evict validator cache entries and terminate live sessions,
	// so the metric names the shared runtime boundary rather than one consumer.
	span.SetAttributes(attribute.Int("auth.runtime_entries_invalidated", invalidated))
	if err := s.publisher.Publish(ctx, *revocation); err != nil {
		// The error text may include infrastructure details, so logs and OTEL use
		// only stable classifications and secret-free family identity.
		span.SetAttributes(attribute.String("outcome", string(outcomeRevokedPublishFailed)))
		span.SetStatus(codes.Error, "app-token invalidation publication failed")
		slog.WarnContext(ctx, "App-token revocation committed but peer invalidation was not published",
			slog.String("error_code", "app_token_invalidation_publish_failed"),
			slog.String("app_family_id", appFamilyID.String()))
		return revocation, nil
	}
	span.SetAttributes(attribute.String("outcome", string(outcomeRevokedPublished)))
	return revocation, nil
}

type Worker struct {
	subscription *nats.Subscription
	once         sync.Once
}

// StartWorker creates one ephemeral JetStream consumer per Engine process.
// Every live replica therefore receives each revocation; DeliverNew avoids
// replaying old events into the empty cache of a restarted process.
func StartWorker(js jetStreamSubscriber, invalidator Invalidator) (*Worker, error) {
	if js == nil || invalidator == nil {
		return nil, errors.New("app-token invalidation subscriber dependencies are required")
	}
	subscription, err := js.Subscribe(
		messaging.AppTokenInvalidatedSubject,
		func(message *nats.Msg) { consume(message, invalidator) },
		nats.BindStream(messaging.FusedEngineStream),
		nats.DeliverNew(),
		nats.ManualAck(),
		nats.AckExplicit(),
	)
	if err != nil {
		return nil, fmt.Errorf("subscribe to app-token invalidations: %w", err)
	}
	return &Worker{subscription: subscription}, nil
}

func (w *Worker) Stop() {
	if w == nil || w.subscription == nil {
		return
	}
	w.once.Do(func() { _ = w.subscription.Unsubscribe() })
}

func consume(message *nats.Msg, invalidator Invalidator) {
	_, span := otel.Tracer("engine").Start(context.Background(), "engine.app_token.invalidation.consume")
	defer span.End()
	event, err := decodeEvent(message.Data)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", string(outcomeInvalid)))
		span.SetStatus(codes.Error, "invalid app-token invalidation event")
		slog.Warn("Discarding invalid app-token invalidation event", slog.String("error_code", "app_token_invalidation_invalid"))
		_ = message.Ack()
		return
	}
	invalidated := invalidator.InvalidateToken(event.TokenID)
	span.SetAttributes(
		attribute.String("app.family_id", event.AppFamilyID.String()),
		attribute.Int("auth.runtime_entries_invalidated", invalidated),
		attribute.String("outcome", string(outcomeInvalidated)),
	)
	_ = message.Ack()
}

func decodeEvent(payload []byte) (Event, error) {
	var event Event
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return Event{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Event{}, errors.New("app-token invalidation event must contain one document")
	}
	if err := validateEvent(event); err != nil {
		return Event{}, err
	}
	return event, nil
}

func validateEvent(event Event) error {
	if event.EventID == uuid.Nil || event.TokenID == uuid.Nil || event.AppFamilyID == uuid.Nil || event.RevokedAt.IsZero() {
		return errors.New("app-token invalidation event is incomplete")
	}
	return nil
}

func recordPublish(ctx context.Context, appFamilyID uuid.UUID, result outcome) {
	span := trace.SpanFromContext(ctx)
	attrs := []attribute.KeyValue{attribute.String("outcome", string(result))}
	if appFamilyID != uuid.Nil {
		attrs = append(attrs, attribute.String("app.family_id", appFamilyID.String()))
	}
	span.AddEvent("engine.app_token.invalidation.publish", trace.WithAttributes(attrs...))
}
