package sandbox

import (
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/Usefused/engine/internal/shared/messaging"
)

// setupNATSSubscriptions initializes NATS JetStream and subscribes to sandbox control topics.
func setupNATSSubscriptions(natsClient *messaging.NATSClient) {
	// Kill and cleanup signals are transient control-plane messages. Subscribe via
	// Core NATS so sandbox restarts do not replay retained JetStream history.
	setupCoreNATSSubscriptions(natsClient)

}

// setupCoreNATSSubscriptions configures subscriptions using fallback Core NATS.
func setupCoreNATSSubscriptions(natsClient *messaging.NATSClient) {
	_, err := natsClient.Subscribe(messaging.FusedEngineKillSubscribe, handleKillMessage)
	if err != nil {
		slog.Error("Failed to subscribe to fused_engine.kill via Core NATS", slog.Any("error", err))
	}
}

// handleKillMessage cancels active MCP sessions for a given SDK ID.
func handleKillMessage(msg *nats.Msg) {
	parts := strings.Split(msg.Subject, ".")
	if len(parts) != 3 {
		// Ignore malformed kill messages.
		return
	}
	appIDHex := parts[2]

	KillMCPSessionsForSDK(appIDHex)
	purgeKillCommand(msg.Subject)

	slog.Info("Sandbox killed via NATS (streams cancelled)", slog.String("sandbox_id", appIDHex))
}

// KillMCPSessionsForSDK cancels every live MCP session for appIDHex,
// terminating each session's spawned process. Exported so the Engine's
// direct deactivate/delete HTTP handlers (internal/engine/api) can call it
// in-process instead of round-tripping through a NATS publish to reach the
// same effect handleKillMessage above achieves for the Registry-driven kill
// path -- one implementation, two triggers, per DRY.
func KillMCPSessionsForSDK(appIDHex string) {
	mcpSessions.Lock()
	defer mcpSessions.Unlock()
	for sessionID, sess := range mcpSessions.m {
		if sess.appID == appIDHex {
			// Cancel the context to kill the spawned process.
			sess.cancel()
			delete(mcpSessions.m, sessionID)
		}
	}
}

func purgeKillCommand(subject string) {
	if globalNATSClient == nil || globalNATSClient.JS == nil {
		return
	}

	if err := globalNATSClient.JS.PurgeStream(messaging.FusedEngineStream, &nats.StreamPurgeRequest{Subject: subject}); err != nil {
		slog.Warn("Failed to purge handled fused_engine.kill command from JetStream", slog.Any("error", err), slog.String("subject", subject))
	}
}
