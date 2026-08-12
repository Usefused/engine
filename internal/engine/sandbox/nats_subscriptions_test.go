package sandbox

import (
	"sync"
	"testing"
)

// TestKillMCPSessionsForSDK_CancelsOnlyMatchingSessions is the direct-call
// path exercised by app-version hard deactivation: it calls
// this function in-process (rather than round-tripping through NATS) to
// force-kill every live MCP session for a deactivated/deleted appID, without
// touching sessions belonging to other SDKs.
func TestKillMCPSessionsForSDK_CancelsOnlyMatchingSessions(t *testing.T) {
	var mu sync.Mutex
	targetCancelled := false
	otherCancelled := false

	mcpSessions.Lock()
	mcpSessions.m["sess-target"] = &mcpSession{
		appID:     "sdk-target",
		sessionID: "sess-target",
		cancel: func() {
			mu.Lock()
			targetCancelled = true
			mu.Unlock()
		},
	}
	mcpSessions.m["sess-other"] = &mcpSession{
		appID:     "sdk-other",
		sessionID: "sess-other",
		cancel: func() {
			mu.Lock()
			otherCancelled = true
			mu.Unlock()
		},
	}
	mcpSessions.Unlock()
	t.Cleanup(func() {
		mcpSessions.Lock()
		delete(mcpSessions.m, "sess-target")
		delete(mcpSessions.m, "sess-other")
		mcpSessions.Unlock()
	})

	KillMCPSessionsForSDK("sdk-target")

	mu.Lock()
	defer mu.Unlock()
	if !targetCancelled {
		t.Error("expected the matching session's context to be cancelled")
	}
	if otherCancelled {
		t.Error("expected a session for a different appID to be left alone")
	}

	mcpSessions.RLock()
	_, targetStillTracked := mcpSessions.m["sess-target"]
	_, otherStillTracked := mcpSessions.m["sess-other"]
	mcpSessions.RUnlock()
	if targetStillTracked {
		t.Error("expected the killed session to be removed from mcpSessions")
	}
	if !otherStillTracked {
		t.Error("expected the untouched session to remain tracked")
	}
}
