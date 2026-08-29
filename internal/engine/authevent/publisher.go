// Package authevent publishes credential-free connected-auth lifecycle transitions for internal Fused consumers.
package authevent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/strictjson"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// SchemaVersion identifies the event envelope contract consumed from the shared stream.
const SchemaVersion = 1

const maxAuthEventResourceCount = 10000

// ErrPublisherUnavailable reports missing Engine startup wiring without changing committed auth state.
var ErrPublisherUnavailable = errors.New("auth event publisher unavailable")

// Type is the closed lifecycle vocabulary internal consumers may handle without parsing failure prose.
type Type string

const (
	// TypeConnectionCompleted announces that a callback grant and its resources committed together.
	TypeConnectionCompleted Type = "connection.completed"
	// TypeTokenRefreshed announces that rotated token material won its refresh lease.
	TypeTokenRefreshed Type = "token.refreshed"
	// TypeTokenRefreshFailed announces that retry state for a transient provider failure committed.
	TypeTokenRefreshFailed Type = "token.refresh_failed"
	// TypeReconnectRequired announces that a connection cannot execute until fresh consent.
	TypeReconnectRequired Type = "connection.reconnect_required"
)

// Event contains routing identity and bounded state only; provider credential material never enters this stream.
type Event struct {
	ID               uuid.UUID  `json:"id"`
	Type             Type       `json:"type"`
	OccurredAt       time.Time  `json:"occurred_at"`
	ConnectionID     uuid.UUID  `json:"connection_id"`
	ConnectSessionID *uuid.UUID `json:"connect_session_id,omitempty"`
	BucketID         uuid.UUID  `json:"bucket_id"`
	ServiceID        uuid.UUID  `json:"service_id"`
	ServiceVersionID uuid.UUID  `json:"service_version_id"`
	CreatedByAppID   *uuid.UUID `json:"created_by_app_id,omitempty"`
	EndUserRef       string     `json:"end_user_ref"`
	AuthType         string     `json:"auth_type"`
	AuthName         string     `json:"auth_name"`
	RefreshState     string     `json:"refresh_state"`
	FailureCode      string     `json:"failure_code,omitempty"`
	ResourceCount    int        `json:"resource_count,omitempty"`
}

// Envelope makes schema evolution explicit without requiring consumers to infer it from payload fields.
type Envelope struct {
	SchemaVersion int   `json:"schema_version"`
	Event         Event `json:"event"`
}

// Decode admits one exact internal envelope before a routing worker trusts its relational identity.
func Decode(payload []byte) (Event, error) {
	var envelope Envelope
	// Strict decoding prevents a newer or malformed producer from being projected under the current public contract.
	if err := strictjson.Decode(payload, &envelope, "auth event"); err != nil {
		return Event{}, fmt.Errorf("decode auth event: %w", err)
	}
	// Schema changes require an explicit projector update instead of silently dropping or misreading fields.
	if envelope.SchemaVersion != SchemaVersion {
		return Event{}, errors.New("auth event schema version is invalid")
	}
	// Publisher validation is repeated at consumption because durable subjects can outlive one process version.
	if err := validate(envelope.Event); err != nil {
		return Event{}, err
	}
	return envelope.Event, nil
}

// WebhookEventName maps the internal closed vocabulary onto its reserved public SDK namespace.
func WebhookEventName(eventType Type) (string, bool) {
	// Only admitted lifecycle types may become generated SDK handler names.
	if !validType(eventType) {
		return "", false
	}
	return "fused.auth." + string(eventType), true
}

// IsWebhookEventName admits only public names produced from the closed internal lifecycle vocabulary.
func IsWebhookEventName(eventName string) bool {
	const prefix = "fused.auth."
	// Exact prefix removal prevents provider labels that merely contain the reserved namespace from being admitted.
	if !strings.HasPrefix(eventName, prefix) {
		return false
	}
	return validType(Type(strings.TrimPrefix(eventName, prefix)))
}

type jetStreamPublisher interface {
	PublishMsgJS(*nats.Msg) (*nats.PubAck, error)
}

// Publisher owns strict validation and JetStream publication for the internal auth lifecycle.
type Publisher struct {
	js jetStreamPublisher
}

// NewPublisher returns nil when boot has no JetStream client, preserving explicit unavailable behavior at Publish.
func NewPublisher(js jetStreamPublisher) *Publisher {
	// A nil publisher cannot truthfully accept lifecycle events.
	if js == nil {
		return nil
	}
	return &Publisher{js: js}
}

// NewConnectionCompleted describes one callback grant only after credential and resource persistence commits.
func NewConnectionCompleted(session store.ConnectSession, connection store.AuthConnection, resourceCount int, occurredAt time.Time) Event {
	event := newConnectionEvent(TypeConnectionCompleted, connection, occurredAt)
	event.ConnectSessionID = optionalUUID(session.ID)
	event.ResourceCount = resourceCount
	return event
}

// NewTokenRefreshed describes one lease-owned token rotation after its compare-and-set commit succeeds.
func NewTokenRefreshed(connection store.AuthConnection, occurredAt time.Time) Event {
	return newConnectionEvent(TypeTokenRefreshed, connection, occurredAt)
}

// NewTokenRefreshFailed describes one retryable refresh failure whose bounded code and retry state were persisted.
func NewTokenRefreshFailed(connection store.AuthConnection, failureCode string, occurredAt time.Time) Event {
	event := newConnectionEvent(TypeTokenRefreshFailed, connection, occurredAt)
	event.FailureCode = failureCode
	event.RefreshState = "ok"
	return event
}

// NewReconnectRequired describes a permanent refresh failure that moved the connection into reconnect-required state.
func NewReconnectRequired(connection store.AuthConnection, failureCode string, occurredAt time.Time) Event {
	event := newConnectionEvent(TypeReconnectRequired, connection, occurredAt)
	event.FailureCode = failureCode
	event.RefreshState = "reconnect_required"
	return event
}

// newConnectionEvent projects only stable connection identity shared by every auth lifecycle transition.
func newConnectionEvent(eventType Type, connection store.AuthConnection, occurredAt time.Time) Event {
	// Producer time defaults locally so tests and older call sites cannot create an unusable event clock.
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	return Event{
		ID: uuid.New(), Type: eventType, OccurredAt: occurredAt.UTC(),
		ConnectionID: connection.ID, BucketID: connection.BucketID,
		ServiceID: connection.ServiceID, ServiceVersionID: connection.ServiceVersionID,
		CreatedByAppID: optionalUUID(connection.CreatedByAppID), EndUserRef: connection.EndUserRef,
		AuthType: connection.AuthType, AuthName: connection.AuthName,
		RefreshState: defaultRefreshState(connection.RefreshState),
	}
}

// optionalUUID omits absent attribution from the wire contract instead of serializing the all-zero UUID.
func optionalUUID(value uuid.UUID) *uuid.UUID {
	// Zero UUIDs are historical absence markers rather than valid internal routing identities.
	if value == uuid.Nil {
		return nil
	}
	copy := value
	return &copy
}

// defaultRefreshState normalizes pre-refresh callback rows to the current healthy state vocabulary.
func defaultRefreshState(state string) string {
	// Successful callback and refresh rows historically omit the explicit in-memory value before persistence projection.
	if strings.TrimSpace(state) == "" {
		return "ok"
	}
	return state
}

// Publish writes one validated event with an idempotent JetStream message identity.
func (publisher *Publisher) Publish(ctx context.Context, event Event) error {
	// Invalid producer state is rejected before serialization or broker I/O.
	if err := validate(event); err != nil {
		recordPublish(ctx, event, "invalid")
		return err
	}
	// An unavailable boot dependency must remain observable without pretending publication succeeded.
	if publisher == nil || publisher.js == nil {
		recordPublish(ctx, event, "unavailable")
		return ErrPublisherUnavailable
	}
	payload, err := json.Marshal(Envelope{SchemaVersion: SchemaVersion, Event: event})
	// Serialization failure remains distinct from broker availability in trace outcomes.
	if err != nil {
		recordPublish(ctx, event, "marshal_failed")
		return fmt.Errorf("marshal auth event: %w", err)
	}
	message := nats.NewMsg(messaging.EngineAuthEventsSubject)
	message.Data = payload
	// Event identity, rather than connection identity, de-duplicates only a retry of this exact transition.
	message.Header.Set(nats.MsgIdHdr, event.ID.String())
	// JetStream acknowledgement is required before reporting successful publication.
	if _, err := publisher.js.PublishMsgJS(message); err != nil {
		recordPublish(ctx, event, "publish_failed")
		return fmt.Errorf("publish auth event: %w", err)
	}
	recordPublish(ctx, event, "published")
	return nil
}

// validate rejects incomplete or contradictory lifecycle documents before they enter the durable stream.
func validate(event Event) error {
	// Relational identity is admitted independently from user-controlled selector strings.
	if err := validateIdentity(event); err != nil {
		return err
	}
	// Selector validation bounds values before any consumer uses them for targeted routing.
	if err := validateSelector(event); err != nil {
		return err
	}
	// Resource counts are bounded before publication to prevent malformed producers from growing consumer work.
	if event.ResourceCount < 0 || event.ResourceCount > maxAuthEventResourceCount {
		return errors.New("auth event resource count is invalid")
	}
	return validateTypeFields(event)
}

// validateIdentity requires the stable relational keys consumers use for targeted delivery.
func validateIdentity(event Event) error {
	// Core routing identity lets a future worker authorize and correlate without loading all connections into memory.
	if event.ID == uuid.Nil || event.ConnectionID == uuid.Nil || event.BucketID == uuid.Nil || event.ServiceID == uuid.Nil || event.ServiceVersionID == uuid.Nil || event.OccurredAt.IsZero() {
		return errors.New("auth event identity is incomplete")
	}
	// Optional app attribution is either absent or a real immutable version ID.
	if event.CreatedByAppID != nil && *event.CreatedByAppID == uuid.Nil {
		return errors.New("auth event app attribution is invalid")
	}
	return nil
}

// validateSelector bounds the exact connected-auth slot and user reference used for routing.
func validateSelector(event Event) error {
	// Connected grants always have a bounded customer reference and exact named OAuth/OIDC slot.
	if strings.TrimSpace(event.EndUserRef) == "" || len(event.EndUserRef) > 255 || strings.TrimSpace(event.AuthType) == "" || len(event.AuthType) > 64 || strings.TrimSpace(event.AuthName) == "" || len(event.AuthName) > 255 {
		return errors.New("auth event connection selector is invalid")
	}
	return nil
}

// validateTypeFields keeps event-specific requirements separate from common identity admission.
func validateTypeFields(event Event) error {
	switch event.Type {
	case TypeConnectionCompleted:
		// Browser callback completion has a one-time session correlation requirement.
		return validateConnectionCompleted(event)
	case TypeTokenRefreshed:
		// Successful rotation admits no failure-only fields.
		return validateTokenRefreshed(event)
	case TypeTokenRefreshFailed:
		// Retryable provider failure keeps the grant usable until expiry.
		return validateTokenRefreshFailed(event)
	case TypeReconnectRequired:
		// Permanent provider rejection requires explicit new consent.
		return validateReconnectRequired(event)
	default:
		// Unknown types fail closed so consumers never infer lifecycle meaning.
		return errors.New("auth event type is invalid")
	}
}

// validateConnectionCompleted requires callback identity and a healthy committed grant.
func validateConnectionCompleted(event Event) error {
	// Callback completion must correlate to the exact one-time browser handoff and carry no failure state.
	if event.ConnectSessionID == nil || *event.ConnectSessionID == uuid.Nil || event.RefreshState != "ok" || event.FailureCode != "" {
		return errors.New("connection-completed auth event is invalid")
	}
	return nil
}

// validateTokenRefreshed rejects callback-only and failure-only fields on successful rotation.
func validateTokenRefreshed(event Event) error {
	// Successful rotation cannot carry failure state, callback identity, or callback resource counts.
	if event.ConnectSessionID != nil || event.RefreshState != "ok" || event.FailureCode != "" || event.ResourceCount != 0 {
		return errors.New("token-refreshed auth event is invalid")
	}
	return nil
}

// validateTokenRefreshFailed admits only a retryable healthy-state failure decision.
func validateTokenRefreshFailed(event Event) error {
	// Retryable failures preserve a healthy connection state and require one stable failure code.
	if event.ConnectSessionID != nil || event.RefreshState != "ok" || !validFailureCode(event.FailureCode) || event.ResourceCount != 0 {
		return errors.New("token-refresh-failed auth event is invalid")
	}
	return nil
}

// validateReconnectRequired keeps permanent grant loss distinct from retryable refresh failure.
func validateReconnectRequired(event Event) error {
	// Permanent grant loss is distinct so consumers can request a new OAuth connection exactly once.
	if event.ConnectSessionID != nil || event.RefreshState != "reconnect_required" || !validFailureCode(event.FailureCode) || event.ResourceCount != 0 {
		return errors.New("reconnect-required auth event is invalid")
	}
	return nil
}

// validFailureCode admits only low-cardinality Engine codes suitable for durable consumer decisions.
func validFailureCode(code string) bool {
	code = strings.TrimSpace(code)
	// Empty, prose-like, and unbounded values cannot become consumer decisions or trace dimensions.
	if code == "" || len(code) > 128 {
		return false
	}
	// Character admission prevents punctuation-delimited provider detail from masquerading as a stable code.
	for _, character := range code {
		// Stable codes use the same lowercase token grammar as existing refresh outcomes.
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

// recordPublish adds only bounded lifecycle outcome to the transition's existing trace.
func recordPublish(ctx context.Context, event Event, outcome string) {
	eventType := "invalid"
	// Malformed producer types collapse before becoming a trace dimension.
	if validType(event.Type) {
		eventType = string(event.Type)
	}
	attrs := []attribute.KeyValue{
		attribute.String("auth.event.type", eventType),
		attribute.String("auth.event.outcome", outcome),
	}
	// A stable failure code is useful for refresh debugging without exposing provider or user material.
	if validFailureCode(event.FailureCode) {
		attrs = append(attrs, attribute.String("auth.event.failure_code", event.FailureCode))
	}
	trace.SpanFromContext(ctx).AddEvent("engine.auth.event.publish", trace.WithAttributes(attrs...))
}

// validType keeps the trace vocabulary aligned with the versioned durable contract.
func validType(eventType Type) bool {
	switch eventType {
	case TypeConnectionCompleted, TypeTokenRefreshed, TypeTokenRefreshFailed, TypeReconnectRequired:
		return true
	default:
		// Unknown values are rejected by validation and collapse to one bounded trace label.
		return false
	}
}

var globalPublisher struct {
	sync.RWMutex
	publisher *Publisher
}

// SetPublisher lets Engine boot own the concrete JetStream lifecycle while producers share one contract.
func SetPublisher(publisher *Publisher) {
	globalPublisher.Lock()
	globalPublisher.publisher = publisher
	globalPublisher.Unlock()
}

// Publish is the sole process-wide entrypoint used after durable connected-auth transitions.
func Publish(ctx context.Context, event Event) error {
	globalPublisher.RLock()
	publisher := globalPublisher.publisher
	globalPublisher.RUnlock()
	// Missing startup wiring stays visible on the caller's existing span.
	if publisher == nil {
		recordPublish(ctx, event, "unavailable")
		return ErrPublisherUnavailable
	}
	return publisher.Publish(ctx, event)
}
