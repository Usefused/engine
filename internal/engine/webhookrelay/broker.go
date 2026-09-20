package webhookrelay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// Broker is an authenticated pull adapter over the existing durable WEBHOOKS stream.
type Broker struct {
	Proof ProofStore
	JS    nats.JetStreamContext
	Conn  *nats.Conn
	Key   []byte
}

// Delivery preserves the existing webhook payload while naming the receiver and broker provenance explicitly.
type Delivery struct {
	SubscriptionID uuid.UUID       `json:"subscription_id"`
	ReceiverID     uuid.UUID       `json:"receiver_id"`
	ServiceID      uuid.UUID       `json:"service_id"`
	EventID        string          `json:"event_id"`
	EventName      string          `json:"event_name"`
	Payload        json.RawMessage `json:"payload"`
	Receipt        string          `json:"receipt"`
}

// Pull rechecks current authorization, then scans a bounded batch without exposing sibling-resource payloads.
func (b Broker) Pull(ctx context.Context, installation, id uuid.UUID) (*Delivery, error) {
	a, err := b.Proof.Authorize(ctx, installation, id)
	// Authorization precedes any stream consumer lookup or provider-payload read.
	if err != nil {
		return nil, err
	}
	sub, err := b.subscription(a)
	// An unavailable durable cannot be substituted with a broad transient subscription.
	if err != nil {
		return nil, err
	}
	defer sub.Unsubscribe()
	// Bound scanning work even when this registration serves many other provider resources.
	for range 32 {
		messages, fetchErr := sub.Fetch(1, nats.MaxWait(100*time.Millisecond))
		// No currently available event is a successful empty poll, not a delivery failure.
		if errors.Is(fetchErr, nats.ErrTimeout) {
			return nil, nil
		}
		// Broker outages leave pending messages unacknowledged for retry.
		if fetchErr != nil {
			return nil, fetchErr
		}
		message := messages[0]
		d, matchErr := b.delivery(a, message)
		// Nonmatching provider resources advance only this receiver's private durable cursor.
		if matchErr != nil {
			_ = message.Ack()
			continue
		}
		// Recheck after fetching so revocation during a wait cannot release a payload.
		if _, err = b.Proof.Authorize(ctx, installation, id); err != nil {
			return nil, err
		}
		return d, nil
	}
	return nil, nil
}

// subscription binds a stable receiver durable, with a start boundary at provider proof creation.
func (b Broker) subscription(a Authorization) (*nats.Subscription, error) {
	name := "remote-" + a.ID.String()
	subject := fmt.Sprintf("webhooks.%s.%s.%s.>", a.AccountID, a.ServiceID, strings.ReplaceAll(a.Label, ".", "-"))
	_, err := b.JS.AddConsumer("WEBHOOKS", &nats.ConsumerConfig{
		Durable: name, FilterSubject: subject, AckPolicy: nats.AckExplicitPolicy,
		DeliverPolicy: nats.DeliverByStartTimePolicy, OptStartTime: &a.CreatedAt,
		AckWait: 30 * time.Second, MaxAckPending: 1, MaxWaiting: 4, InactiveThreshold: 24 * time.Hour,
	})
	// Existing durable state is reused, never reset to replay historical provider data.
	if err != nil {
		return nil, err
	}
	return b.JS.PullSubscribe(subject, name, nats.Bind("WEBHOOKS", name), nats.ManualAck())
}

// delivery validates original ingress identity and exact provider resource before creating an audience-bound receipt.
func (b Broker) delivery(a Authorization, m *nats.Msg) (*Delivery, error) {
	// Only the original signed ingress registration may supply exportable provider events.
	if m.Header.Get("X-Fused-Webhook-ID") != a.RegistrationID.String() {
		return nil, ErrDenied
	}
	identity, err := eventIdentity(m.Data, a.Policy, a.ResourceID, a.AppID)
	// A failed ownership match exposes neither payload nor metadata to the remote installation.
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(m.Subject, ".", 5)
	// The shared subject layout must remain structurally intact before naming a public event.
	if len(parts) != 5 || strings.HasPrefix(parts[4], "fused.") {
		return nil, ErrDenied
	}
	d := &Delivery{SubscriptionID: a.ID, ReceiverID: a.ReceiverID, ServiceID: a.ServiceID,
		EventID: Digest(a.RegistrationID.String() + ":" + identity), EventName: parts[4], Payload: delegatedPayload(m.Data, parts[4])}
	d.Receipt = b.signReceipt(a.ID, m.Reply)
	return d, nil
}

// signReceipt authenticates the exact JetStream acknowledgement destination without trusting caller-supplied subjects.
func (b Broker) signReceipt(id uuid.UUID, reply string) string {
	value := id.String() + "\n" + reply
	mac := hmac.New(sha256.New, b.Key)
	_, _ = mac.Write([]byte("fused-webhook-ack-v1\n" + value))
	return base64.RawURLEncoding.EncodeToString([]byte(value)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Acknowledge authorizes the subscription again and accepts only receipts issued for that exact audience.
func (b Broker) Acknowledge(ctx context.Context, installation, id uuid.UUID, receipt string) error {
	// Revoked and expired installations cannot mutate durable receiver state.
	if _, err := b.Proof.Authorize(ctx, installation, id); err != nil {
		return err
	}
	reply, err := b.receiptReply(id, receipt)
	// A forged or cross-receiver receipt cannot become a NATS publish target.
	if err != nil {
		return err
	}
	// JetStream double-ack uses core request/reply, not the stream publication API.
	_, err = b.Conn.RequestWithContext(ctx, reply, []byte("+ACK"))
	return err
}

// receiptReply verifies the receipt before returning its signed internal acknowledgement subject.
func (b Broker) receiptReply(id uuid.UUID, receipt string) (string, error) {
	encoded, signature, ok := strings.Cut(receipt, ".")
	value, e1 := base64.RawURLEncoding.DecodeString(encoded)
	got, e2 := base64.RawURLEncoding.DecodeString(signature)
	// Reject malformed or oversized receipts before they can consume broker resources.
	if !ok || e1 != nil || e2 != nil || len(value) > 1024 {
		return "", ErrDenied
	}
	mac := hmac.New(sha256.New, b.Key)
	_, _ = mac.Write([]byte("fused-webhook-ack-v1\n" + string(value)))
	// Authentication is constant-time and covers the receiver audience as well as the internal subject.
	if !hmac.Equal(got, mac.Sum(nil)) {
		return "", ErrDenied
	}
	audience, reply, ok := strings.Cut(string(value), "\n")
	// Even authenticated receipts must use the exact supported acknowledgement namespace.
	if !ok || audience != id.String() || !strings.HasPrefix(reply, "$JS.ACK.WEBHOOKS.remote-"+id.String()+".") {
		return "", ErrDenied
	}
	return reply, nil
}

// delegatedPayload keeps the ordinary webhook envelope while withholding provider headers, query credentials and broker ingress slugs.
func delegatedPayload(raw []byte, eventName string) json.RawMessage {
	var envelope struct {
		Body json.RawMessage `json:"body"`
	}
	_ = json.Unmarshal(raw, &envelope)
	payload, _ := json.Marshal(map[string]any{"body": envelope.Body, "headers": map[string]string{}, "query": map[string]string{}, "path": map[string]string{"eventName": eventName}})
	return payload
}
