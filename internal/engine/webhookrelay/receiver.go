package webhookrelay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

// Receiver persists local lifecycle and deduplication while retaining the existing WEBHOOKS event stream.
type Receiver struct {
	DB     *pgxpool.Pool
	JS     nats.JetStreamContext
	Client *Client
	Key    []byte
	after  uuid.UUID
}

type receiverTarget struct {
	ManagedApplicationID string
	RegistrationID       uuid.UUID
	ConnectionID         uuid.UUID
	ServiceID            uuid.UUID
	VersionID            uuid.UUID
	AccountID            uuid.UUID
	Label                string
	ConfigHash           string
	Config               Config
	WrappedKey           string
	Ciphertext           string
}

type receiverState struct {
	BrokerURL      string
	ID             uuid.UUID
	SubscriptionID uuid.UUID
	ConfigHash     string
	TokenHash      string
}

// Run reconciles bounded pages; failures retain pending remote cleanup and unacknowledged provider events.
func (w *Receiver) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	// A single loop avoids unbounded per-subscription goroutines and respects Engine shutdown.
	for {
		// Context cancellation must stop work before another provider credential can be used.
		if ctx.Err() != nil {
			return
		}
		// Error logging contains a fixed code, never payloads, URLs, receipts or credentials.
		if err := w.Step(ctx); err != nil {
			slog.WarnContext(ctx, "Webhook relay reconciliation pending", slog.String("error_code", "webhook_relay_unavailable"))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Step serializes reconcilers across Engine replicas with a database transaction-scoped lease.
func (w *Receiver) Step(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tx, err := w.DB.Begin(ctx)
	// A storage outage must not advance remote acknowledgements.
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var locked bool
	err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(619038521)`).Scan(&locked)
	// Only one replica owns local publication and remote acknowledgement at a time.
	if err != nil || !locked {
		return err
	}
	// Pending withdrawals run before any new delivery work.
	if err = w.cleanup(ctx); err != nil {
		return err
	}
	targets, err := w.targets(ctx)
	// An unavailable desired-config read cannot be interpreted as permission to old subscriptions.
	if err != nil {
		return err
	}
	return w.receivePage(ctx, targets)
}

// receivePage bounds per-pass work while keeping a slow or invalid subscription from permanently starving later receivers.
func (w *Receiver) receivePage(ctx context.Context, targets []receiverTarget) error {
	var firstErr error
	// A bounded page prevents one failed receiver from starving unrelated subscriptions.
	for _, target := range targets {
		// A bounded pass yields the replica lease before slow receivers can monopolize it.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.after = target.RegistrationID
		// Preserve the first diagnostic while continuing independent authorized receivers.
		if err := w.receive(ctx, target); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Keyset pagination wraps only after reaching the end of the configured receiver set.
	if len(targets) < 64 {
		w.after = uuid.Nil
	}
	return firstErr
}

// targets resolves active managed connections, source buckets and workspace audience in one bounded SQL query.
func (w *Receiver) targets(ctx context.Context) ([]receiverTarget, error) {
	rows, err := w.DB.Query(ctx, `SELECT wh.id,c.id,wh.service_id,wh.service_version_id,ws.account_id,wh.label,
 encode(sha256(convert_to(wh.relay_config::text,'UTF8')),'hex'),wh.relay_config,c.encrypted_dek,c.access_token,c.managed_application_id
 FROM fused_workspace_webhooks wh
 JOIN fused_auth_connections c ON c.id=(wh.relay_config->'source'->>'connection_id')::uuid
 JOIN fused_buckets b ON b.id=c.bucket_id AND b.name=wh.relay_config->'source'->>'bucket'
 CROSS JOIN fused_workspaces ws WHERE wh.auth_type='fused_remote' AND c.is_managed_auth
 AND c.service_id=wh.service_id AND c.refresh_state='ok'
 AND (c.expires_at IS NULL OR c.expires_at>NOW()) AND wh.id>$1
 ORDER BY wh.id LIMIT 64`, w.after)
	// Invalid desired state or database failure must fail closed.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := []receiverTarget{}
	// Every row already passed the relational connection and service authorization predicates.
	for rows.Next() {
		var t receiverTarget
		var raw []byte
		// Stop before credential use if the persisted row cannot be decoded exactly.
		if err = rows.Scan(&t.RegistrationID, &t.ConnectionID, &t.ServiceID, &t.VersionID, &t.AccountID, &t.Label, &t.ConfigHash, &raw, &t.WrappedKey, &t.Ciphertext, &t.ManagedApplicationID); err != nil {
			return nil, err
		}
		t.Config, err = Decode(raw)
		// Unsupported source policy cannot turn into a broad remote receiver.
		if err != nil || t.Config.Source == nil {
			return nil, ErrDenied
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

// receive ensures explicit subscription ownership before pulling or accepting a broker event.
func (w *Receiver) receive(ctx context.Context, t receiverTarget) error {
	dek, err := store.UnwrapDEK(w.Key, t.WrappedKey)
	// Provider-token decryption failure must not fall back to another connection.
	if err != nil {
		return err
	}
	token, err := store.DecryptWithDEK(dek, t.Ciphertext)
	// No plaintext provider credential is persisted by the relay worker.
	if err != nil {
		return err
	}
	state, err := w.ensureState(ctx, t, receiverTokenIdentity(token, t.ManagedApplicationID))
	// Mismatched or withdrawing state must settle before resubscribing.
	if err != nil {
		return err
	}
	// Persist the remote subscription identity before accepting any event.
	if state.SubscriptionID == uuid.Nil {
		state.SubscriptionID, err = w.Client.Subscribe(ctx, t.Config.Source.RegistrationID, state.ID, token, t.ManagedApplicationID)
		// Missing broker proof requires a fresh provider connection, never a customer workspace claim.
		if err != nil {
			return err
		}
		_, err = w.DB.Exec(ctx, `UPDATE fused_webhook_receivers SET subscription_id=$2 WHERE id=$1`, state.ID, state.SubscriptionID)
		// An uncommitted subscription remains recoverable via the idempotent receiver identity.
		if err != nil {
			return err
		}
	}
	d, err := w.Client.Pull(ctx, state.SubscriptionID)
	// Idle polls are normal; failures retain the broker's pending delivery.
	if err != nil || d == nil {
		return err
	}
	return w.accept(ctx, t, state, *d)
}

// ensureState retains opaque receiver identity across retries and fences config or connection-token replacement.
func (w *Receiver) ensureState(ctx context.Context, t receiverTarget, tokenHash string) (receiverState, error) {
	_, err := w.DB.Exec(ctx, `INSERT INTO fused_webhook_receivers(id,registration_id,connection_id,config_hash,token_hash,subscription_id,broker_url)
 VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(registration_id) DO NOTHING`, uuid.New(), t.RegistrationID, t.ConnectionID, t.ConfigHash, tokenHash, uuid.Nil, w.Client.URL)
	// Failed durable identity creation cannot authorize a transient remote receiver.
	if err != nil {
		return receiverState{}, err
	}
	var s receiverState
	err = w.DB.QueryRow(ctx, `SELECT id,subscription_id,config_hash,token_hash,broker_url FROM fused_webhook_receivers WHERE registration_id=$1`, t.RegistrationID).Scan(&s.ID, &s.SubscriptionID, &s.ConfigHash, &s.TokenHash, &s.BrokerURL)
	// A source replacement must revoke the prior subscription before a new one can deliver.
	if err == nil && (s.ConfigHash != t.ConfigHash || s.TokenHash != tokenHash || s.BrokerURL != w.Client.URL) {
		_, err = w.DB.Exec(ctx, `UPDATE fused_webhook_receivers SET registration_id=NULL WHERE id=$1`, s.ID)
		// The cleanup pass owns durable remote withdrawal before the next setup attempt.
		if err == nil {
			err = ErrDenied
		}
	}
	return s, err
}

// cleanup drains a bounded set of receiver tombstones without deleting another connection's subscriptions.
func (w *Receiver) cleanup(ctx context.Context) error {
	rows, err := w.DB.Query(ctx, `SELECT r.id,r.subscription_id FROM fused_webhook_receivers r
 LEFT JOIN fused_workspace_webhooks w ON w.id=r.registration_id
 WHERE (r.registration_id IS NULL OR r.connection_id IS NULL OR w.relay_config->'source' IS NULL
 OR r.config_hash<>encode(sha256(convert_to(w.relay_config::text,'UTF8')),'hex')) AND r.broker_url=$1 LIMIT 64`, w.Client.URL)
	// Failure to read pending withdrawal must not falsely complete cleanup.
	if err != nil {
		return err
	}
	var pending []receiverState
	// Release the result set before network calls or mutations to avoid retaining a database cursor.
	for rows.Next() {
		var s receiverState
		if err = rows.Scan(&s.ID, &s.SubscriptionID); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, s)
	}
	err = rows.Err()
	rows.Close()
	// Scan errors keep all durable pending-cleanup rows intact.
	if err != nil {
		return err
	}
	// Each request revokes one exact broker subscription, never an account-wide grant.
	for _, s := range pending {
		// Receiver identity also withdraws a subscription whose successful setup response was lost.
		if err = w.Client.RevokeReceiver(ctx, s.ID); err != nil {
			return err
		}
		// Local cleanup follows remote acknowledgement so network failures remain recoverable.
		if _, err = w.DB.Exec(ctx, `DELETE FROM fused_webhook_receivers WHERE id=$1`, s.ID); err != nil {
			return err
		}
	}
	return nil
}

// accept verifies broker audience and current local connection before entering the ordinary consumer stream.
func (w *Receiver) accept(ctx context.Context, t receiverTarget, s receiverState, d Delivery) error {
	// A valid TLS broker response still must name this exact local receiver, service and subscription.
	if d.SubscriptionID != s.SubscriptionID || d.ReceiverID != s.ID || d.ServiceID != t.ServiceID || !validEvent(d) {
		return ErrDenied
	}
	var active bool
	err := w.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM fused_workspace_webhooks wh
 JOIN fused_auth_connections c ON c.id=$2 JOIN fused_webhook_receivers r ON r.registration_id=wh.id
 WHERE wh.id=$1 AND wh.auth_type='fused_remote' AND encode(sha256(convert_to(wh.relay_config::text,'UTF8')),'hex')=$3
 AND c.is_managed_auth AND c.refresh_state='ok' AND c.access_token=$4 AND r.id=$5 AND c.managed_application_id=$6)`, t.RegistrationID, t.ConnectionID, t.ConfigHash, t.Ciphertext, s.ID, t.ManagedApplicationID).Scan(&active)
	// Disconnect, reconnect or config replacement during network I/O wins before local publication.
	if err != nil || !active {
		return ErrDenied
	}
	// Publish first; only a local durable ACK permits advancing the broker's pending delivery.
	if err = w.publish(ctx, t, s, d); err != nil {
		return err
	}
	return w.Client.Ack(ctx, s.SubscriptionID, d.Receipt)
}

// validEvent prevents a compromised wire shape from altering the fixed NATS subject namespace.
func validEvent(d Delivery) bool {
	// Broker receipts and stable provider IDs are mandatory even for non-JSON provider bodies.
	if len(d.EventID) != 64 || len(d.Payload) > 2<<20 || d.Receipt == "" || d.EventName == "" {
		return false
	}
	// The remote event name cannot inject wildcard, whitespace or reserved auth subjects.
	if strings.ContainsAny(d.EventName, "*> \t\r\n") || strings.HasPrefix(d.EventName, "fused.") {
		return false
	}
	return json.Valid(d.Payload)
}

// publish deduplicates completed imports durably and uses JetStream's stable message ID for crash-window retries.
func (w *Receiver) publish(ctx context.Context, t receiverTarget, s receiverState, d Delivery) error {
	var exists bool
	err := w.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM fused_webhook_relay_receipts WHERE registration_id=$1 AND event_id=$2)`, t.RegistrationID, d.EventID).Scan(&exists)
	// A known committed import acknowledges retries without republishing to SDK subscribers.
	if err != nil || exists {
		return err
	}
	msg := nats.NewMsg(fmt.Sprintf("webhooks.%s.%s.%s.%s", t.AccountID, t.ServiceID, strings.ReplaceAll(t.Label, ".", "-"), d.EventName))
	msg.Data = d.Payload
	msg.Header.Set(nats.MsgIdHdr, t.RegistrationID.String()+":"+d.EventID)
	msg.Header.Set("X-Webhook-Msg-ID", d.EventID)
	msg.Header.Set("X-Fused-Webhook-ID", t.RegistrationID.String())
	msg.Header.Set("X-Fused-Service-Version-ID", t.VersionID.String())
	msg.Header.Set("X-Webhook-Start-Time", time.Now().Format(time.RFC3339Nano))
	msg.Header.Set("X-Fused-Webhook-Provenance", "broker-verified")
	msg.Header.Set("X-Fused-Webhook-Subscription-ID", s.SubscriptionID.String())
	// Provider headers remain payload context; only these Engine-owned metadata fields describe delegated verification.
	if _, err = w.JS.PublishMsg(msg, nats.Context(ctx)); err != nil {
		return err
	}
	_, err = w.DB.Exec(ctx, `INSERT INTO fused_webhook_relay_receipts(registration_id,event_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, t.RegistrationID, d.EventID)
	return err
}

// receiverTokenIdentity fences app replacement even if a provider issues coincident token strings across applications.
func receiverTokenIdentity(token, applicationID string) string {
	// Default receivers retain their existing durable hash during additive upgrades.
	if applicationID == "" {
		return Digest(token)
	}
	return Digest(token + "\x00" + applicationID)
}
