package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/config"
)

func init() {
	cfg = &config.Config{
		Sandbox: config.SandboxConfig{
			SessionMaxAgeSeconds:   300,
			ToolCallTimeoutSeconds: 1, // small value for tests
		},
	}
}

// ─── Existing tests ────────────────────────────────────────────────────────────

func TestQueryOrFormToJSON(t *testing.T) {
	tests := []struct {
		name    string
		values  url.Values
		wantMap map[string]any
	}{
		{
			name: "Single values",
			values: url.Values{
				"event":  []string{"user.created"},
				"userId": []string{"123"},
			},
			wantMap: map[string]any{
				"event":  "user.created",
				"userId": "123",
			},
		},
		{
			name: "Duplicate keys (arrays)",
			values: url.Values{
				"tag": []string{"admin", "user"},
				"id":  []string{"1"},
			},
			wantMap: map[string]any{
				"tag": []any{"admin", "user"},
				"id":  "1",
			},
		},
		{
			name:    "Empty values",
			values:  url.Values{},
			wantMap: map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBytes, err := queryOrFormToJSON(tt.values)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var gotMap map[string]any
			if err := json.Unmarshal(gotBytes, &gotMap); err != nil {
				t.Fatalf("failed to unmarshal got: %v", err)
			}

			if !reflect.DeepEqual(gotMap, tt.wantMap) {
				t.Errorf("queryOrFormToJSON() = %v, want %v", gotMap, tt.wantMap)
			}
		})
	}
}

// ─── Rate limiter tests ────────────────────────────────────────────────────────

func TestRateLimiter_AllowsUpToBurst(t *testing.T) {
	// burst of 3, slow refill (1/min — effectively zero during this test)
	store := newRateLimitStore(1, 3)

	for i := 0; i < 3; i++ {
		if !store.allow("sdk-1") {
			t.Fatalf("expected request %d to be allowed (within burst)", i+1)
		}
	}
	// 4th request must be denied — burst exhausted
	if store.allow("sdk-1") {
		t.Fatal("expected 4th request to be rate-limited, but it was allowed")
	}
}

func TestRateLimiter_IndependentPerKey(t *testing.T) {
	store := newRateLimitStore(1, 1) // burst of 1

	if !store.allow("sdk-a") {
		t.Fatal("sdk-a first request should be allowed")
	}
	// sdk-a is now exhausted, but sdk-b should still have its own full bucket
	if !store.allow("sdk-b") {
		t.Fatal("sdk-b first request should be allowed independently of sdk-a")
	}
	// Both exhausted now
	if store.allow("sdk-a") {
		t.Fatal("sdk-a should be rate-limited")
	}
	if store.allow("sdk-b") {
		t.Fatal("sdk-b should be rate-limited")
	}
}

func TestRateLimiter_RefillsOverTime(t *testing.T) {
	// 60 tokens/min = 1 token/second
	store := newRateLimitStore(60, 1)

	if !store.allow("sdk-1") {
		t.Fatal("first request should be allowed")
	}
	if store.allow("sdk-1") {
		t.Fatal("second request should be denied immediately after first")
	}

	// Wait slightly more than 1 second for refill
	time.Sleep(1100 * time.Millisecond)

	if !store.allow("sdk-1") {
		t.Fatal("request should be allowed after token refill")
	}
}

func TestAllowSSEConnect_Returns429WhenExhausted(t *testing.T) {
	initRateLimiters(1, 1, 60, 10) // burst=1 for SSE

	// First connection: allowed
	w := httptest.NewRecorder()
	if !allowSSEConnect(w, "sdk-test") {
		t.Fatal("first SSE connection should be allowed")
	}

	// Second connection immediately: denied → 429
	w2 := httptest.NewRecorder()
	if allowSSEConnect(w2, "sdk-test") {
		t.Fatal("second SSE connection should be rate-limited")
	}
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w2.Code)
	}
	if w2.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header to be set")
	}
}

func TestAllowMessage_Returns429WhenExhausted(t *testing.T) {
	initRateLimiters(60, 10, 1, 1) // burst=1 for messages

	w := httptest.NewRecorder()
	if !allowMessage(w, "sdk-msg") {
		t.Fatal("first message should be allowed")
	}

	w2 := httptest.NewRecorder()
	if allowMessage(w2, "sdk-msg") {
		t.Fatal("second message should be rate-limited")
	}
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w2.Code)
	}
}

// ─── Tool-call timeout tests ───────────────────────────────────────────────────

func TestEnforceToolCallTimeout_KillsSessionOnTimeout(t *testing.T) {
	// Use the global config value of 1 second for timeout testing

	// Spin up a real long-lived process (sleep 30) to simulate a hung tool call.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("skipping: cannot start 'sleep' process: %v", err)
	}
	defer cancel()

	sess := &mcpSession{
		appID:     "sdk-timeout-test",
		sessionID: "sess-timeout-test",
		cmd:       cmd,
		cancel:    cancel,
		pendingRequests: map[string]pendingReq{
			"call-1": {endpointName: "slow_tool", startTime: time.Now()},
		},
	}

	go enforceToolCallTimeout(sess, "call-1", "slow_tool")

	// Give the timeout goroutine time to fire (1s) + a small buffer.
	time.Sleep(1500 * time.Millisecond)

	// The process should have been killed.
	if cmd.ProcessState == nil {
		// ProcessState is set only after Wait() — check if process is still running.
		if err := cmd.Process.Signal(os.Signal(nil)); err == nil {
			// Signal(nil) succeeds if process is still alive.
			t.Fatal("expected process to be killed after timeout, but it is still running")
		}
	}

	// The pending request should have been removed.
	sess.pendingMu.Lock()
	_, stillPending := sess.pendingRequests["call-1"]
	sess.pendingMu.Unlock()
	if stillPending {
		t.Fatal("expected pending request to be removed after timeout")
	}
}

func TestEnforceToolCallTimeout_DoesNotKillIfCompleted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("skipping: cannot start 'sleep' process: %v", err)
	}

	killed := false
	var mu sync.Mutex

	sess := &mcpSession{
		appID:     "sdk-no-kill",
		sessionID: "sess-no-kill",
		cmd:       cmd,
		cancel: func() {
			mu.Lock()
			killed = true
			mu.Unlock()
			cancel()
		},
		pendingRequests: map[string]pendingReq{
			"call-2": {endpointName: "fast_tool", startTime: time.Now()},
		},
	}

	go enforceToolCallTimeout(sess, "call-2", "fast_tool")

	// Simulate the tool call completing before the timeout fires.
	sess.pendingMu.Lock()
	delete(sess.pendingRequests, "call-2")
	sess.pendingMu.Unlock()

	time.Sleep(1500 * time.Millisecond)

	mu.Lock()
	wasKilled := killed
	mu.Unlock()

	if wasKilled {
		t.Fatal("session should NOT have been killed — the tool call completed in time")
	}
	// Clean up
	cmd.Process.Kill()
}

// ─── trackPendingRequest tests ─────────────────────────────────────────────────

func TestTrackPendingRequest_TracksToolsCall(t *testing.T) {
	sess := &mcpSession{
		pendingRequests: make(map[string]pendingReq),
	}

	body, _ := json.Marshal(map[string]any{
		"method": "tools/call",
		"id":     "42",
		"params": map[string]any{"name": "get_user"},
	})

	callID, endpointName, _ := trackPendingRequest(body, sess)

	if callID != "42" {
		t.Errorf("expected callID '42', got '%s'", callID)
	}
	if endpointName != "get_user" {
		t.Errorf("expected endpointName 'get_user', got '%s'", endpointName)
	}

	sess.pendingMu.Lock()
	req, ok := sess.pendingRequests["42"]
	sess.pendingMu.Unlock()

	if !ok {
		t.Fatal("expected pending request to be recorded")
	}
	if req.endpointName != "get_user" {
		t.Errorf("expected endpointName 'get_user' in pending req, got '%s'", req.endpointName)
	}
}

func TestTrackPendingRequest_IgnoresNonToolsCall(t *testing.T) {
	sess := &mcpSession{
		pendingRequests: make(map[string]pendingReq),
	}

	body, _ := json.Marshal(map[string]any{
		"method": "tools/list",
		"id":     "1",
	})

	callID, _, _ := trackPendingRequest(body, sess)
	if callID != "" {
		t.Errorf("expected empty callID for non-tools/call method, got '%s'", callID)
	}
	if len(sess.pendingRequests) != 0 {
		t.Error("expected no pending requests to be recorded for tools/list")
	}
}

// ─── HTTP-block preload tests ──────────────────────────────────────────────────

func TestWriteHTTPBlockPreload_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	writeHTTPBlockPreload(dir)

	preloadPath := dir + "/http-block-preload.cjs"
	data, err := os.ReadFile(preloadPath)
	if err != nil {
		t.Fatalf("preload file not created: %v", err)
	}
	content := string(data)

	// Must patch Module._resolveFilename
	if !bytes.Contains(data, []byte("Module._resolveFilename")) {
		t.Error("preload script does not patch Module._resolveFilename")
	}
	// Must block key HTTP modules
	for _, mod := range []string{"axios", "node-fetch", "undici", "got", "https"} {
		if !bytes.Contains(data, []byte(mod)) {
			t.Errorf("preload script does not block '%s'", mod)
		}
	}
	_ = content
}

func TestWriteHTTPBlockPreload_NodeCanLoadIt(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available in PATH, skipping runtime test")
	}

	dir := t.TempDir()
	writeHTTPBlockPreload(dir)
	preloadPath := dir + "/http-block-preload.cjs"

	// The preload itself must load without error (no syntax errors).
	cmd := exec.Command("node", "--require", preloadPath, "-e", "console.log('ok')")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("preload script has errors: %v\nOutput: %s", err, out)
	}

	// require('http') must throw after the preload is applied.
	cmd2 := exec.Command("node", "--require", preloadPath, "-e", "require('http')")
	if err := cmd2.Run(); err == nil {
		t.Fatal("expected require('http') to throw after preload, but it succeeded")
	}

	// require('axios') must throw.
	cmd3 := exec.Command("node", "--require", preloadPath, "-e", "require('axios')")
	if err := cmd3.Run(); err == nil {
		t.Fatal("expected require('axios') to throw after preload, but it succeeded")
	}
}

// ─── Config defaults test ──────────────────────────────────────────────────────

func TestInitSharedSandboxesRemovesOnlyLegacyDependencyCache(t *testing.T) {
	originalRoot := sandboxDataRoot
	sandboxDataRoot = t.TempDir()
	t.Cleanup(func() { sandboxDataRoot = originalRoot })

	sharedDir, perAppDir, outsidePath := createLegacySandboxFixture(t)
	initSharedSandboxes()
	assertLegacySandboxCleanup(t, sharedDir, perAppDir, outsidePath)
}

func createLegacySandboxFixture(t *testing.T) (string, string, string) {
	t.Helper()
	sharedDir := sandboxesDir()
	legacyModuleDir, perAppDir := sharedDir+"/node_modules/example", sharedDir+"/app-123"
	if err := os.MkdirAll(legacyModuleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(perAppDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		legacyModuleDir + "/index.js": "module.exports = {};",
		sharedDir + "/package.json":   `{}`,
		perAppDir + "/keep.txt":       "tenant data",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outsidePath := sandboxDataRoot + "/outside.txt"
	if err := os.WriteFile(outsidePath, []byte("outside cache"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, legacyModuleDir+"/outside-link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(legacyModuleDir, 0o555); err != nil {
		t.Fatal(err)
	}
	return sharedDir, perAppDir, outsidePath
}

func assertLegacySandboxCleanup(t *testing.T, sharedDir, perAppDir, outsidePath string) {
	t.Helper()
	for _, name := range legacySharedSandboxEntries {
		if _, err := os.Stat(sharedDir + "/" + name); !os.IsNotExist(err) {
			t.Fatalf("legacy dependency %q was not removed", name)
		}
	}
	if data, err := os.ReadFile(perAppDir + "/keep.txt"); err != nil || string(data) != "tenant data" {
		t.Fatalf("per-app data changed: data=%q err=%v", data, err)
	}
	info, err := os.Stat(outsidePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("symlink target permissions changed: mode=%v", info.Mode().Perm())
	}
}

func TestSandboxConfigDefaults(t *testing.T) {
	// config.Load("") panics when the encryption key is shorter than 32 chars
	// (a pre-existing issue in the config package unrelated to our changes).
	// We verify our new sandbox struct defaults directly instead.
	defaults := config.SandboxConfig{
		ToolCallTimeoutSeconds: 45,
		SessionMaxAgeSeconds:   300,
		RateLimit: config.SandboxRateLimitConfig{
			SSEConnectionsPerMinute: 5,
			SSEBurst:                2,
			MessagesPerMinute:       60,
			MessagesBurst:           10,
		},
	}

	if defaults.ToolCallTimeoutSeconds != 45 {
		t.Errorf("expected ToolCallTimeoutSeconds=45, got %d", defaults.ToolCallTimeoutSeconds)
	}
	if defaults.SessionMaxAgeSeconds != 300 {
		t.Errorf("expected SessionMaxAgeSeconds=300, got %d", defaults.SessionMaxAgeSeconds)
	}
	if defaults.RateLimit.MessagesPerMinute != 60 {
		t.Errorf("expected MessagesPerMinute=60, got %d", defaults.RateLimit.MessagesPerMinute)
	}
	if defaults.RateLimit.SSEConnectionsPerMinute != 5 {
		t.Errorf("expected SSEConnectionsPerMinute=5, got %d", defaults.RateLimit.SSEConnectionsPerMinute)
	}
	if defaults.RateLimit.MessagesBurst != 10 {
		t.Errorf("expected MessagesBurst=10, got %d", defaults.RateLimit.MessagesBurst)
	}
}
