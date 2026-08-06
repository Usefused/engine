package sandbox

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/Usefused/engine/internal/shared/messaging"
)

// setupNATSSubscriptions initializes NATS JetStream and subscribes to sandbox control topics.
func setupNATSSubscriptions(natsClient *messaging.NATSClient) {
	// Kill and cleanup signals are transient control-plane messages. Subscribe via
	// Core NATS so sandbox restarts do not replay retained JetStream history.
	setupCoreNATSSubscriptions(natsClient)

	setupCleanupSubscription(natsClient)
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

// setupCleanupSubscription binds to the fused_engine.cleanup topic to remove sandbox files from disk.
func setupCleanupSubscription(natsClient *messaging.NATSClient) {
	_, err := natsClient.Subscribe(messaging.FusedEngineCleanupSubscribe, func(msg *nats.Msg) {
		parts := strings.Split(msg.Subject, ".")
		if len(parts) != 3 {
			// Ignore malformed cleanup messages.
			return
		}
		appIDHex := parts[2]

		if err := CleanupMCPSandboxDir(appIDHex); err != nil {
			slog.Error("Failed to remove sandbox directory via NATS", slog.Any("error", err), slog.String("sandbox_id", appIDHex))
			return
		}
		slog.Info("Sandbox directory cleaned up via NATS message", slog.String("sandbox_id", appIDHex))
	})
	if err != nil {
		slog.Error("Failed to subscribe to fused_engine.cleanup.*", slog.Any("error", err))
	}
}

// CleanupMCPSandboxDir removes the per-SDK sandbox directory from disk.
// Exported for the same reason as KillMCPSessionsForSDK above -- the
// direct DELETE /sdk-config/{id} handler calls this in-process rather than
// publishing a NATS message to itself.
func CleanupMCPSandboxDir(appIDHex string) error {
	return os.RemoveAll(sandboxDirFor(appIDHex))
}

// sandboxDataRoot is the parent of the "data/sandboxes" tree, relative to
// the Engine process's working directory by default -- matches how
// initSharedSandboxes (sandbox.go) has always resolved it. It's a
// package-level var, not a const, solely so tests can point it at a
// t.TempDir() (see nats_subscriptions_test.go): directory-cleanup tests
// need a location the test process actually owns and can unlink from,
// which a fixed relative path under the repo checkout is not guaranteed to
// be in every environment this test suite runs in.
var sandboxDataRoot = "."

// sandboxesDir is the shared parent directory every per-SDK sandbox working
// directory lives under -- the single source of truth this file,
// mcp_cleanup.go, and sandbox.go all resolve their sandbox paths from
// (previously each hardcoded "./data/sandboxes" independently).
func sandboxesDir() string {
	return filepath.Join(sandboxDataRoot, "data", "sandboxes")
}

// sandboxDirFor is the per-SDK sandbox working directory for appIDHex.
func sandboxDirFor(appIDHex string) string {
	return filepath.Join(sandboxesDir(), appIDHex)
}
