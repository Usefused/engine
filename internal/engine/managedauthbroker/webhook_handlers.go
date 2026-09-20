package managedauthbroker

import (
	"encoding/json"
	"net/http"

	"github.com/Usefused/engine/internal/engine/webhookrelay"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// MountWebhookRoutes exposes only installation-authenticated pull transport; no receiver URLs or provider-resource claims are accepted.
func MountWebhookRoutes(router chi.Router, broker *webhookrelay.Broker, installations *Store) {
	router.Route("/managed-auth/broker/webhooks", func(r chi.Router) {
		r.Use(requireInstallationAccessToken(installations))
		r.Post("/subscriptions", relaySubscribeHandler(broker))
		r.Post("/subscriptions/{subscriptionID}/pull", relayPullHandler(broker))
		r.Post("/subscriptions/{subscriptionID}/ack", relayAckHandler(broker))
		r.Delete("/subscriptions/{subscriptionID}", relayRevokeHandler(broker))
		r.Delete("/receivers/{subscriptionID}", relayRevokeReceiverHandler(broker))
	})
}

// relaySubscribeHandler binds explicit consent to a token the broker exchanged for this installation.
func relaySubscribeHandler(b *webhookrelay.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			RegistrationID uuid.UUID `json:"registration_id"`
			ReceiverID     uuid.UUID `json:"receiver_id"`
			AccessToken    string    `json:"access_token"`
		}
		// Small strict requests exclude alternate resource identities and arbitrary remote destinations.
		if !decodeRelayRequest(w, r, &input) {
			return
		}
		installation := r.Context().Value(installationContextKey{}).(Installation)
		id, err := b.Proof.Subscribe(r.Context(), installation.ID, input.RegistrationID, input.ReceiverID, input.AccessToken)
		// Identical denial copy prevents token or provider-resource enumeration across tenants.
		if err != nil {
			writeBrokerError(w, 403, "webhook subscription is unavailable")
			return
		}
		writeBrokerJSON(w, 200, map[string]uuid.UUID{"subscription_id": id})
	}
}

// relayIdentity obtains both caller and audience from already authenticated routing state.
func relayIdentity(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "subscriptionID"))
	// Malformed audience IDs cannot fall back to an installation-wide stream.
	if err != nil {
		writeBrokerError(w, 400, "invalid subscription")
		return uuid.Nil, uuid.Nil, false
	}
	return r.Context().Value(installationContextKey{}).(Installation).ID, id, true
}

// relayPullHandler returns only the broker-authorized provider payload with explicit delegated provenance.
func relayPullHandler(b *webhookrelay.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		installation, id, ok := relayIdentity(w, r)
		// Invalid routing identity terminates before stream access.
		if !ok {
			return
		}
		delivery, err := b.Pull(r.Context(), installation, id)
		// Do not return raw storage errors or provider payloads on failure.
		if err != nil {
			writeBrokerError(w, 403, "webhook delivery is unavailable")
			return
		}
		// Empty polls are normal for private Engines with no new matching provider event.
		if delivery == nil {
			w.WriteHeader(204)
			return
		}
		writeBrokerJSON(w, 200, delivery)
	}
}

// relayAckHandler advances only a currently authorized audience with its broker-issued receipt.
func relayAckHandler(b *webhookrelay.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		installation, id, ok := relayIdentity(w, r)
		// Invalid routing identity terminates before acknowledgement processing.
		if !ok {
			return
		}
		var input struct {
			Receipt string `json:"receipt"`
		}
		// Receipts are bounded opaque capabilities, not caller-selected NATS subjects.
		if !decodeRelayRequest(w, r, &input) {
			return
		}
		// Current authorization and receipt authentication must both succeed.
		if err := b.Acknowledge(r.Context(), installation, id, input.Receipt); err != nil {
			writeBrokerError(w, 403, "webhook acknowledgement is unavailable")
			return
		}
		w.WriteHeader(204)
	}
}

// relayRevokeHandler makes withdrawal durable before best-effort removal of the existing stream consumer.
func relayRevokeHandler(b *webhookrelay.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		installation, id, ok := relayIdentity(w, r)
		// Invalid routing identity cannot select a durable consumer.
		if !ok {
			return
		}
		// Tombstones stop delivery even while JetStream is unavailable.
		if err := b.Proof.Revoke(r.Context(), installation, id); err != nil {
			writeBrokerError(w, 503, "webhook withdrawal unavailable")
			return
		}
		// Consumer deletion is performed by broker retention; callers cannot delete an unowned durable by guessing its ID.
		w.WriteHeader(204)
	}
}

// decodeRelayRequest bounds credential-bearing input and refuses undeclared ownership or destination fields.
func decodeRelayRequest(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	// Unknown fields must never be interpreted as a future authorization override.
	if err := d.Decode(out); err != nil {
		writeBrokerError(w, 400, "invalid webhook request")
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	return true
}

// relayRevokeReceiverHandler withdraws every grant bound to one caller-owned durable receiver, including ambiguous setup results.
func relayRevokeReceiverHandler(b *webhookrelay.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		installation, id, ok := relayIdentity(w, r)
		// A malformed receiver must never widen a withdrawal to the installation.
		if !ok {
			return
		}
		// Withdrawal checks installation ownership in SQL and remains idempotent for unknown receiver IDs.
		if err := b.Proof.RevokeReceiver(r.Context(), installation, id); err != nil {
			writeBrokerError(w, 503, "webhook withdrawal unavailable")
			return
		}
		w.WriteHeader(204)
	}
}
