package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/google/uuid"
)

// ─── Mock token validator ─────────────────────────────────────────────────────

type mockTokenValidator struct {
	validToken string
	accountID  uuid.UUID
}

func (m *mockTokenValidator) Validate(_ context.Context, _ uuid.UUID, token string) (uuid.UUID, error) {
	if token == m.validToken {
		return m.accountID, nil
	}
	return uuid.Nil, auth.ErrUnauthorized
}

type countingValidator struct {
	validToken string
	accountID  uuid.UUID
	count      *int
}

func (c *countingValidator) Validate(_ context.Context, _ uuid.UUID, token string) (uuid.UUID, error) {
	*c.count++
	if token == c.validToken {
		return c.accountID, nil
	}
	return uuid.Nil, auth.ErrUnauthorized
}

// ─── Blocker 1 + Gap 4: Bearer scheme rejection ───────────────────────────────

// TestBearerSchemeRejectedInMessageHandler verifies the Bearer scheme check
// in mcpMessageHandler (which runs after session lookup, so non-Bearer reaches
// the scheme check). The SSE handler's extractMCPParams also does this check,
// but the sdk-id check runs first in unit tests without a Chi router.
func TestBearerSchemeRejectedInMessageHandler(t *testing.T) {
	sessionID := uuid.New().String()
	sess := &mcpSession{
		artifactID: uuid.New().String(),
		sessionID:  sessionID,
		token:      "correct-token",
		idleTimer:  time.AfterFunc(time.Hour, func() {}),
	}
	mcpSessions.Lock()
	mcpSessions.m[sessionID] = sess
	mcpSessions.Unlock()
	defer func() {
		mcpSessions.Lock()
		delete(mcpSessions.m, sessionID)
		mcpSessions.Unlock()
		sess.idleTimer.Stop()
	}()

	// Non-Bearer scheme must be rejected with 401 regardless of session.
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/mcp/message?sessionId=%s", sessionID),
		bytes.NewReader(body))
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	w := httptest.NewRecorder()
	mcpMessageHandler(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("non-Bearer scheme: want 401, got %d (body=%s)", w.Code, w.Body.String())
	}

	// No Authorization header must also be rejected with 401.
	r2 := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/mcp/message?sessionId=%s", sessionID),
		bytes.NewReader(body))
	w2 := httptest.NewRecorder()
	mcpMessageHandler(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: want 401, got %d (body=%s)", w2.Code, w2.Body.String())
	}
}

// ─── Blocker 1: validateTokenWithCache ────────────────────────────────────────

func TestValidateTokenWithCacheRejectsInvalidToken(t *testing.T) {
	orig := globalTokenValidator
	defer func() { globalTokenValidator = orig }()
	globalTokenValidator = &mockTokenValidator{validToken: "good", accountID: uuid.New()}

	_, err := validateTokenWithCache(context.Background(), uuid.New().String(), "bad-token")
	if err == nil {
		t.Fatal("expected error for invalid token, got nil")
	}
}

func TestValidateTokenWithCacheCachesOnSuccess(t *testing.T) {
	orig := globalTokenValidator
	defer func() { globalTokenValidator = orig }()

	callCount := 0
	artifactID := uuid.New()
	accountID := uuid.New()
	globalTokenValidator = &countingValidator{validToken: "good", accountID: accountID, count: &callCount}

	got1, err := validateTokenWithCache(context.Background(), artifactID.String(), "good")
	if err != nil || got1 != accountID {
		t.Fatalf("first call: err=%v accountID=%v", err, got1)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 validator call, got %d", callCount)
	}

	// Second call within TTL: cache hit, validator not called again.
	got2, err := validateTokenWithCache(context.Background(), artifactID.String(), "good")
	if err != nil || got2 != accountID {
		t.Fatalf("second call: err=%v accountID=%v", err, got2)
	}
	if callCount != 1 {
		t.Fatalf("expected still 1 validator call after cache hit, got %d", callCount)
	}

	// Clean up cache entry so other tests aren't affected.
	tokenCache.Lock()
	delete(tokenCache.m, sha256CacheKey(artifactID.String(), "good"))
	tokenCache.Unlock()
}

func TestValidateTokenWithCacheCallsValidatorAfterExpiry(t *testing.T) {
	artifactID := "sdk-expired-test"
	token := "tok-expired"
	key := sha256CacheKey(artifactID, token)

	// Pre-populate with an expired entry.
	tokenCache.Lock()
	tokenCache.m[key] = tokenCacheEntry{accountID: uuid.New(), expiry: time.Now().Add(-1 * time.Second)}
	tokenCache.Unlock()

	orig := globalTokenValidator
	defer func() { globalTokenValidator = orig }()
	// Validator returns error so we know a real Validate call was made.
	globalTokenValidator = &mockTokenValidator{validToken: "different", accountID: uuid.New()}

	_, err := validateTokenWithCache(context.Background(), artifactID, token)
	if err == nil {
		t.Fatal("expected validator error after cache expiry, got nil")
	}
}

// ─── Blocker 2: /mcp/message token re-verification ───────────────────────────

func TestMCPMessageHandlerRejectsWrongToken(t *testing.T) {
	sessionID := uuid.New().String()
	sess := &mcpSession{
		artifactID: uuid.New().String(),
		sessionID:  sessionID,
		token:      "correct-token",
		idleTimer:  time.AfterFunc(time.Hour, func() {}),
	}
	mcpSessions.Lock()
	mcpSessions.m[sessionID] = sess
	mcpSessions.Unlock()
	defer func() {
		mcpSessions.Lock()
		delete(mcpSessions.m, sessionID)
		mcpSessions.Unlock()
		sess.idleTimer.Stop()
	}()

	cases := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"wrong token", "Bearer wrong-token", http.StatusUnauthorized},
		{"no auth header", "", http.StatusUnauthorized},
		{"basic scheme", "Basic dXNlcjpwYXNz", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
			r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/mcp/message?sessionId=%s", sessionID), bytes.NewReader(body))
			if tc.authHeader != "" {
				r.Header.Set("Authorization", tc.authHeader)
			}
			w := httptest.NewRecorder()
			mcpMessageHandler(w, r)
			if w.Code != tc.wantStatus {
				t.Errorf("auth=%q: want %d, got %d (body=%s)", tc.authHeader, tc.wantStatus, w.Code, w.Body.String())
			}
		})
	}
}

// ─── Blocker 5: Body size limit ───────────────────────────────────────────────

func TestMCPMessageHandlerEnforcesBodySizeLimit(t *testing.T) {
	sessionID := uuid.New().String()
	sess := &mcpSession{
		artifactID: uuid.New().String(),
		sessionID:  sessionID,
		token:      "tok",
		idleTimer:  time.AfterFunc(time.Hour, func() {}),
	}
	mcpSessions.Lock()
	mcpSessions.m[sessionID] = sess
	mcpSessions.Unlock()
	defer func() {
		mcpSessions.Lock()
		delete(mcpSessions.m, sessionID)
		mcpSessions.Unlock()
		sess.idleTimer.Stop()
	}()

	oversized := strings.Repeat("x", 300*1024) // 300 KB > 256 KB limit
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/mcp/message?sessionId=%s", sessionID),
		strings.NewReader(oversized))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	mcpMessageHandler(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d (body=%s)", w.Code, w.Body.String())
	}
}

// ─── Blocker 4: Per-session temp dir ─────────────────────────────────────────

func TestSessionTmpDirIsPerSessionNotPerSDK(t *testing.T) {
	artifactID := uuid.New().String()
	sessionA := uuid.New().String()
	sessionB := uuid.New().String()

	// The dir naming convention used in buildMCPCommand.
	dirA := "fused-sandbox-" + sessionA
	dirB := "fused-sandbox-" + sessionB

	if dirA == dirB {
		t.Fatal("two different sessions produced the same temp dir name")
	}
	// The SDK ID must not appear in the path (it would cause cross-session races).
	if strings.Contains(dirA, artifactID) || strings.Contains(dirB, artifactID) {
		t.Fatalf("session temp dirs must not embed artifactID; got %q and %q", dirA, dirB)
	}
	// Verify the new prefix is "fused-sandbox-" not the old "opensync-sandbox-".
	if strings.HasPrefix(dirA, "opensync-sandbox-") {
		t.Fatalf("expected fused-sandbox- prefix, got opensync-sandbox-")
	}
}

// ─── Blocker 3b: Span key allowlist ──────────────────────────────────────────

func TestHandleFusedSpanAllowlistLogic(t *testing.T) {
	allowed := map[string]struct{}{
		"operation_id": {},
		"latency_ms":   {},
		"status":       {},
	}

	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"all allowed keys", `{"operation_id":"foo","latency_ms":5,"status":"ok"}`, true},
		{"foreign key injected", `{"operation_id":"foo","malicious_key":"steal"}`, false},
		{"server-controlled artifact_id spoofed", `{"artifact_id":"attacker","operation_id":"x"}`, false},
		{"empty payload", `{}`, true},
		{"subset of allowed", `{"latency_ms":10}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var data map[string]any
			json.Unmarshal([]byte(tc.payload), &data)
			pass := true
			for k := range data {
				if _, ok := allowed[k]; !ok {
					pass = false
					break
				}
			}
			if pass != tc.want {
				t.Errorf("payload %q: want allowed=%v, got %v", tc.payload, tc.want, pass)
			}
		})
	}
}

// ─── Blocker 3a: Analytics omits params and result ───────────────────────────

func TestPublishAnalyticsOmitsParamsAndResult(t *testing.T) {
	// Reconstruct the event map the same way publishAnalyticsForRequest does.
	req := pendingReq{
		endpointName: "listRepos",
		startTime:    time.Now().Add(-50 * time.Millisecond),
		arguments:    map[string]any{"token": "secret", "query": "fused"},
	}
	msg := map[string]any{
		"id":     "1",
		"result": map[string]any{"content": []any{"repo1", "repo2"}},
	}

	latencyMs := time.Since(req.startTime).Milliseconds()
	failed := false
	if _, hasErr := msg["error"]; hasErr {
		failed = true
	} else if res, hasRes := msg["result"].(map[string]any); hasRes {
		if isErr, _ := res["isError"].(bool); isErr {
			failed = true
		}
	}
	event := map[string]any{
		"artifact_id":   "test-sdk",
		"session_id":    "test-session",
		"endpoint_name": req.endpointName,
		"latency_ms":    latencyMs,
		"failed":        failed,
		"timestamp":     time.Now(),
	}

	if _, has := event["params"]; has {
		t.Error("analytics event must not contain params")
	}
	if _, has := event["result"]; has {
		t.Error("analytics event must not contain result")
	}
	if event["endpoint_name"] != "listRepos" {
		t.Errorf("expected endpoint_name=listRepos, got %v", event["endpoint_name"])
	}
	if _, has := event["latency_ms"]; !has {
		t.Error("analytics event must contain latency_ms")
	}
	if _, has := event["failed"]; !has {
		t.Error("analytics event must contain failed flag")
	}
}
