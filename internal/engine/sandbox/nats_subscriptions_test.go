package sandbox

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestKillMCPSessionsForSDK_CancelsOnlyMatchingSessions is the direct-call
// path exercised by api.DeactivateSDKHandler/DeleteSDKHandler: they call
// this function in-process (rather than round-tripping through NATS) to
// force-kill every live MCP session for a deactivated/deleted artifactID, without
// touching sessions belonging to other SDKs.
func TestKillMCPSessionsForSDK_CancelsOnlyMatchingSessions(t *testing.T) {
	var mu sync.Mutex
	targetCancelled := false
	otherCancelled := false

	mcpSessions.Lock()
	mcpSessions.m["sess-target"] = &mcpSession{
		artifactID: "sdk-target",
		sessionID:  "sess-target",
		cancel: func() {
			mu.Lock()
			targetCancelled = true
			mu.Unlock()
		},
	}
	mcpSessions.m["sess-other"] = &mcpSession{
		artifactID: "sdk-other",
		sessionID:  "sess-other",
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
		t.Error("expected a session for a different artifactID to be left alone")
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

// TestCleanupMCPSandboxDir_RemovesDirectory is the on-disk half of
// api.DeleteSDKHandler's best-effort cleanup: it must remove the sandbox
// working directory for the given artifactID, and must not error when the
// directory never existed (delete-before-first-connect is a valid case).
func TestCleanupMCPSandboxDir_RemovesDirectory(t *testing.T) {
	const artifactIDHex = "sdk-cleanup-test"
	// Redirect the sandbox root at a directory the test process actually
	// owns and can unlink from -- t.TempDir() (not the repo checkout, which
	// some environments mount read/write-but-not-delete) -- and restore it
	// so other tests in this package keep resolving the real "./data"
	// convention.
	previousRoot := sandboxDataRoot
	sandboxDataRoot = t.TempDir()
	t.Cleanup(func() { sandboxDataRoot = previousRoot })

	dir := sandboxDirFor(artifactIDHex)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to set up sandbox dir fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("failed to write fixture file: %v", err)
	}

	if err := CleanupMCPSandboxDir(artifactIDHex); err != nil {
		t.Fatalf("CleanupMCPSandboxDir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected sandbox dir to be removed, stat err = %v", err)
	}
}

func TestCleanupMCPSandboxDir_NoOpWhenDirMissing(t *testing.T) {
	if err := CleanupMCPSandboxDir("sdk-never-existed"); err != nil {
		t.Fatalf("expected no error removing a non-existent sandbox dir, got %v", err)
	}
}
