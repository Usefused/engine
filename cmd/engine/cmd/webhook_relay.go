package cmd

import (
	"context"
	"log/slog"

	"github.com/Usefused/engine/internal/engine/managedauthbroker"
	"github.com/Usefused/engine/internal/engine/webhookrelay"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// startWebhookRelay shares the existing Engine database, installation credentials and webhook stream.
func startWebhookRelay(ctx context.Context, database *pgxpool.Pool, natsClient *messaging.NATSClient, key []byte, broker managedAuthBrokerDeps, consumer managedAuthClientDeps, brokerURL string) *webhookrelay.Broker {
	var relay *webhookrelay.Broker
	// Only the explicitly enabled broker can issue provider-response proof and serve remote subscriptions.
	if broker.connect != nil {
		proof := webhookrelay.ProofStore{DB: database}
		broker.connect.Proof = &proof
		relay = &webhookrelay.Broker{Proof: proof, JS: natsClient.JS, Conn: natsClient.Conn, Key: key}
	}
	// Self-hosted consumers need only their existing Registry-advertised broker enrollment.
	if consumer.service != nil {
		client, err := webhookrelay.NewClient(brokerURL, consumer.service, nil)
		// Invalid transport configuration must disable delegated input rather than exposing credentials.
		if err != nil {
			slog.ErrorContext(ctx, "Webhook relay transport unavailable")
			return relay
		}
		worker := &webhookrelay.Receiver{DB: database, JS: natsClient.JS, Client: client, Key: key}
		go worker.Run(ctx)
	}
	return relay
}

// mountWebhookRelayRoutes keeps broker routes absent on ordinary consumer Engines.
func mountWebhookRelayRoutes(r chi.Router, deps engineRouterDeps) {
	// A nil adapter must never mount an unauthenticated fallback.
	if deps.managedWebhookBroker != nil {
		managedauthbroker.MountWebhookRoutes(r, deps.managedWebhookBroker, deps.managedAuthInstalls)
	}
}
