package bridgehost

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/shared/messaging"
	bridgev1 "github.com/Usefused/engine/pkg/bridge/v1"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
	"log/slog"
	"strings"
	"time"
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
	var raw []byte
	err := s.repo.Pool.QueryRow(ctx, `SELECT m.document FROM fused_bridge_pairings p JOIN fused_bridge_manifests m ON m.pairing_id=p.id AND m.profile_id=$3 AND m.hash=$4 WHERE p.id=$1 AND p.account_id=$2 AND p.revoked_at IS NULL AND p.scope='workspace' AND p.inventory_generation=p.generation AND EXISTS(SELECT 1 FROM fused_bridge_profiles c WHERE c.pairing_id=p.id AND c.profile_id=$3 AND c.state<>'removed') AND `+ownerAuthorized, pin.PairingID, account, pin.ProfileID, pin.ManifestHash).Scan(&raw)
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
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := r.Pool.Begin(ctx)
	// A worker without a transaction cannot reserve a delivery across replicas.
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var account, pairing, destination, eventID string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT p.account_id,e.pairing_id,e.destination_id,e.event_id,e.envelope FROM fused_bridge_events e JOIN fused_bridge_pairings p ON p.id=e.pairing_id WHERE e.delivered_at IS NULL AND p.revoked_at IS NULL AND p.scope='workspace' AND p.inventory_generation=p.generation AND EXISTS(SELECT 1 FROM fused_bridge_profiles c WHERE c.pairing_id=p.id AND c.profile_id=e.profile_id AND c.state<>'removed') AND `+ownerAuthorized+` ORDER BY e.accepted_at FOR UPDATE OF e SKIP LOCKED LIMIT 1`).Scan(&account, &pairing, &destination, &eventID, &raw)
	// Competing workers, pending inventory reconciliation, or an empty outbox are ordinary idle states.
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	event := &bridgev1.Event{}
	// Corrupt envelopes remain retained for diagnosis rather than being silently discarded.
	if proto.Unmarshal(raw, event) != nil {
		return false, ErrContract
	}
	pin := Pin{PairingID: pairing, ProfileID: event.ProfileId, ManifestHash: event.ManifestHash, ConnectionID: event.ConnectionId, OperationID: event.OperationId, ContractHash: event.ContractHash}
	message := nats.NewMsg(EventSubject(account, pin))
	message.Data = event.PayloadJson
	id := Digest([]byte(pairing + "\n" + destination + "\n" + eventID))
	message.Header.Set(nats.MsgIdHdr, id)
	message.Header.Set("X-Webhook-Msg-ID", id)
	message.Header.Set("X-Bridge-Event-ID", eventID)
	// Lost broker acknowledgements keep the same message ID for safe at-least-once retry.
	if _, err = js.PublishMsg(message, nats.Context(ctx)); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE fused_bridge_events SET delivered_at=now(),envelope=''::bytea WHERE pairing_id=$1 AND destination_id=$2 AND event_id=$3`, pairing, destination, eventID)
	// Keep the small content-hash tombstone so late Bridge retries cannot recreate a settled event.
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
