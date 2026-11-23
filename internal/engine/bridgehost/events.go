package bridgehost

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/shared/messaging"
	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

// EventName binds receiver delivery to the exact original manifest and operation contract.
func EventName(pin Pin) string {
	raw, _ := json.Marshal([]string{pin.ProfileID, pin.ManifestHash, pin.ConnectionID, pin.OperationID, pin.ContractHash})
	return "bridge." + strings.TrimPrefix(Digest(raw), "sha256:")
}

// EventSubject isolates local ingress by account, installation and exact immutable contract.
func EventSubject(account string, pin Pin) string {
	return "webhooks." + account + ".fused-bridge." + pin.PairingID + "." + strings.TrimPrefix(EventName(pin), "bridge.")
}

// AuthorizeEventPin retains historical delivery while rechecking live installation and profile authority.
func (s *Server) AuthorizeEventPin(ctx context.Context, account string, pin Pin) error {
	raw, err := s.repo.EventManifestDocument(ctx, account, pin)
	// Offline installations may drain accepted events; a reconnect must reconcile deletions before delivery resumes.
	if err != nil {
		return ErrDenied
	}
	var doc ManifestDocument
	// Persisted bytes must still describe an event, never a callable operation.
	if json.Unmarshal(raw, &doc) != nil {
		return ErrContract
	}
	contract, err := operation(doc, pin.ConnectionID, pin.OperationID, pin.ContractHash)
	// Reject non-event contracts so unary operations cannot be subscribed as webhook streams.
	if err != nil || contract.Kind != "event" {
		return ErrContract
	}
	return nil
}

// StartEventDelivery hands committed ingress to the existing durable SDK receiver stream.
func (s *Server) StartEventDelivery(ctx context.Context, client *messaging.NATSClient) error {
	// Ingress acceptance must not depend on a live consumer; the retained stream owns subsequent delivery.
	if err := client.InitStream("WEBHOOKS", []string{"webhooks.>"}); err != nil {
		return err
	}
	// One bounded worker per replica cooperates through row locks rather than a process-local ownership flag.
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			// Shutdown interrupts idle polling without abandoning an accepted event.
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			for i := 0; i < 100; i++ {
				moved, err := s.repo.DeliverEvent(ctx, client.JS)
				// Broker/database failures retain the durable outbox and back off until the next poll.
				if err != nil {
					slog.WarnContext(ctx, "Bridge event handoff deferred", "error_code", "bridge_delivery_unavailable")
					break
				}
				// An empty outbox does not justify repeated database reads within one tick.
				if !moved {
					break
				}
			}
		}
	}()
	return nil
}

// DeliverEvent atomically claims one accepted envelope and clears its payload only after broker acknowledgement.
func (r *Repository) DeliverEvent(ctx context.Context, js nats.JetStreamContext) (bool, error) {
	return r.claimAndAckPendingEvent(ctx, func(txCtx context.Context, item pendingBridgeEvent) error {
		event := &bridgev1.Event{}
		// Corrupt envelopes remain retained for diagnosis rather than being silently discarded.
		if proto.Unmarshal(item.Envelope, event) != nil {
			return ErrContract
		}
		pin := Pin{PairingID: item.Pairing, ProfileID: event.ProfileId, ManifestHash: event.ManifestHash, ConnectionID: event.ConnectionId, OperationID: event.OperationId, ContractHash: event.ContractHash}
		message := nats.NewMsg(EventSubject(item.Account, pin))
		message.Data = event.PayloadJson
		id := Digest([]byte(item.Pairing + "\n" + item.Destination + "\n" + item.EventID))
		message.Header.Set(nats.MsgIdHdr, id)
		message.Header.Set("X-Webhook-Msg-ID", id)
		message.Header.Set("X-Bridge-Event-ID", item.EventID)
		// Lost broker acknowledgements keep the same message ID for safe at-least-once retry.
		_, err := js.PublishMsg(message, nats.Context(txCtx))
		return err
	})
}
