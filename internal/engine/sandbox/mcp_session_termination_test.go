package sandbox

import (
	"sync"
	"testing"
)

// TestTerminateMCPSessionsForAppCancelsOnlyMatchingSessions exercises the
// in-process hard-deactivation path without touching another app's sessions.
func TestTerminateMCPSessionsForAppCancelsOnlyMatchingSessions(t *testing.T) {
	var mu sync.Mutex
	targetCancelled := false
	otherCancelled := false

	mcpSessions.Lock()
	mcpSessions.m["sess-target"] = &mcpSession{
		appID: "sdk-target", sessionID: "sess-target",
		cancel: func() {
			mu.Lock()
			targetCancelled = true
			mu.Unlock()
		},
	}
	mcpSessions.m["sess-other"] = &mcpSession{
		appID: "sdk-other", sessionID: "sess-other",
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

	TerminateMCPSessionsForApp("sdk-target")

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
		t.Error("expected the terminated session to be removed")
	}
	if !otherStillTracked {
		t.Error("expected the unrelated session to remain tracked")
	}
}
