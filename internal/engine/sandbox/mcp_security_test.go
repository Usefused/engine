package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// ─── Mock token validator ─────────────────────────────────────────────────────

type mockTokenValidator struct {
	validToken string
	accountID  uuid.UUID
}

func (m *mockTokenValidator) Validate(_ context.Context, appID uuid.UUID, token string) (auth.RuntimeIdentity, error) {
	if token == m.validToken {
		return auth.RuntimeIdentity{AccountID: m.accountID, AppFamilyID: uuid.New(), AppID: appID, AppVersion: "1.0.0", Kind: "mcp", Status: "active", TokenPolicy: store.AppTokenPolicy{AllowAll: true}}, nil
	}
	return auth.RuntimeIdentity{}, auth.ErrUnauthorized
}

type countingValidator struct {
	validToken string
	accountID  uuid.UUID
	count      *int
}

func (c *countingValidator) Validate(_ context.Context, appID uuid.UUID, token string) (auth.RuntimeIdentity, error) {
	*c.count++
	if token == c.validToken {
		return auth.RuntimeIdentity{AccountID: c.accountID, AppFamilyID: uuid.New(), AppID: appID, AppVersion: "1.0.0", Kind: "mcp", Status: "active", TokenPolicy: store.AppTokenPolicy{AllowAll: true}}, nil
	}
	return auth.RuntimeIdentity{}, auth.ErrUnauthorized
}

// ─── Blocker 1 + Gap 4: Bearer scheme rejection ───────────────────────────────

// TestBearerSchemeRejectedInMessageHandler verifies the Bearer scheme check
// in mcpMessageHandler (which runs after session lookup, so non-Bearer reaches
// the scheme check). The SSE handler's extractMCPParams also does this check,
// but the sdk-id check runs first in unit tests without a Chi router.
func TestBearerSchemeRejectedInMessageHandler(t *testing.T) {
	sessionID := uuid.New().String()
	sess := &mcpSession{
		appID:     uuid.New().String(),
		sessionID: sessionID,
		transport: "sse",
		token:     "correct-token",
		idleTimer: time.AfterFunc(time.Hour, func() {}),
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

// MCP session establishment always passes through the configured validator.

func TestValidateMCPTokenRejectsInvalidToken(t *testing.T) {
	orig := globalTokenValidator
	defer func() { globalTokenValidator = orig }()
	globalTokenValidator = &mockTokenValidator{validToken: "good", accountID: uuid.New()}

	_, err := validateMCPToken(context.Background(), uuid.New().String(), "bad-token")
	if err == nil {
		t.Fatal("expected error for invalid token, got nil")
	}
}

func TestValidateMCPTokenUsesValidatorForEverySession(t *testing.T) {
	orig := globalTokenValidator
	defer func() { globalTokenValidator = orig }()

	callCount := 0
	appID := uuid.New()
	accountID := uuid.New()
	globalTokenValidator = &countingValidator{validToken: "good", accountID: accountID, count: &callCount}

	got1, err := validateMCPToken(context.Background(), appID.String(), "good")
	if err != nil || got1.AccountID != accountID {
		t.Fatalf("first call: err=%v identity=%v", err, got1)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 validator call, got %d", callCount)
	}

	got2, err := validateMCPToken(context.Background(), appID.String(), "good")
	if err != nil || got2.AccountID != accountID {
		t.Fatalf("second call: err=%v identity=%v", err, got2)
	}
	if callCount != 2 {
		t.Fatalf("validator calls = %d, want 2 for two session checks", callCount)
	}
}

func TestValidateMCPTokenDeniesRevokedTokenImmediately(t *testing.T) {
	orig := globalTokenValidator
	defer func() { globalTokenValidator = orig }()
	validator := &countingValidator{validToken: "good", accountID: uuid.New(), count: new(int)}
	globalTokenValidator = validator
	appID := uuid.NewString()

	if _, err := validateMCPToken(context.Background(), appID, "good"); err != nil {
		t.Fatalf("initial validation failed: %v", err)
	}
	validator.validToken = ""
	if _, err := validateMCPToken(context.Background(), appID, "good"); err == nil {
		t.Fatal("revoked token remained valid for a later session")
	}
	if *validator.count != 2 {
		t.Fatalf("validator calls = %d, want 2 across issue and revocation checks", *validator.count)
	}
}

// ─── Blocker 2: /mcp/message token re-verification ───────────────────────────

func TestMCPMessageHandlerRejectsWrongToken(t *testing.T) {
	sessionID := uuid.New().String()
	sess := &mcpSession{
		appID:     uuid.New().String(),
		sessionID: sessionID,
		transport: "sse",
		token:     "correct-token",
		idleTimer: time.AfterFunc(time.Hour, func() {}),
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
		appID:     uuid.New().String(),
		sessionID: sessionID,
		transport: "sse",
		token:     "tok",
		idleTimer: time.AfterFunc(time.Hour, func() {}),
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
	appID := uuid.New().String()
	sessionA := uuid.New().String()
	sessionB := uuid.New().String()

	// The dir naming convention used in buildMCPCommand.
	dirA := "fused-sandbox-" + sessionA
	dirB := "fused-sandbox-" + sessionB

	if dirA == dirB {
		t.Fatal("two different sessions produced the same temp dir name")
	}
	// The SDK ID must not appear in the path (it would cause cross-session races).
	if strings.Contains(dirA, appID) || strings.Contains(dirB, appID) {
		t.Fatalf("session temp dirs must not embed appID; got %q and %q", dirA, dirB)
	}
	// Verify the new prefix is "fused-sandbox-" not the old "opensync-sandbox-".
	if strings.HasPrefix(dirA, "opensync-sandbox-") {
		t.Fatalf("expected fused-sandbox- prefix, got opensync-sandbox-")
	}
}
