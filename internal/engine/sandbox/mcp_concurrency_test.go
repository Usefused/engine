package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// mcpSSEConcurrencyRequest builds an HTTP request wired with a chi route context
// and an Authorization header so mcpSseHandler can resolve parameters.
func mcpSSEConcurrencyRequest(appIDHex, token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/mcp/sse", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", appIDHex)
	req = req.WithContext(appendChiRouteContext(req.Context(), rctx))
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func appendChiRouteContext(ctx context.Context, rctx *chi.Context) context.Context {
	return context.WithValue(ctx, chi.RouteCtxKey, rctx)
}

// preseedMCPSessions inserts n fake sessions into the global mcpSessions map.
// Cleanup removes them after the test.
func preseedMCPSessions(t *testing.T, n int) {
	t.Helper()
	mcpSessions.Lock()
	for i := 0; i < n; i++ {
		sid := uuid.NewString()
		mcpSessions.m[sid] = &mcpSession{
			appID:     uuid.NewString(),
			sessionID: sid,
			token:     "tok-" + sid,
			idleTimer: time.AfterFunc(time.Hour, func() {}),
		}
	}
	mcpSessions.Unlock()
	t.Cleanup(func() {
		mcpSessions.Lock()
		// Remove only the ones we added in this call. Since the test package
		// controls all pre-seeding, the simplest safe approach is to clear
		// every entry that was added (tracking via a slice is possible, but
		// tests run sequentially and cleanup is idempotent).
		for k := range mcpSessions.m {
			delete(mcpSessions.m, k)
		}
		mcpSessions.Unlock()
	})
}

// TestMCPSSE_Concurrency_BlocksWhenAtLimit verifies the P4-05 gate:
// when activeMCPSessionCount() is at MaxSandboxConcurrency the SSE
// handler must return HTTP 402 before any token validation or cache wiring.
func TestMCPSSE_Concurrency_BlocksWhenAtLimit(t *testing.T) {
	installDirectMCPRouteResolver(t)
	withEntitlement(t, models.RuntimeEntitlement{MaxSandboxConcurrency: models.IntPtr(2)})
	preseedMCPSessions(t, 2)

	req := mcpSSEConcurrencyRequest(uuid.NewString(), "any-token")
	rec := httptest.NewRecorder()
	mcpSseHandler(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402 at limit, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !containsString(rec.Body.String(), "limit reached") {
		t.Fatalf("expected 'limit reached' in body, got %s", rec.Body.String())
	}
}

// TestMCPSSE_Concurrency_ZeroBlocksAll verifies the P4-05 gate:
// a MaxSandboxConcurrency of 0 means no sessions are allowed.
func TestMCPSSE_Concurrency_ZeroBlocksAll(t *testing.T) {
	installDirectMCPRouteResolver(t)
	withEntitlement(t, models.RuntimeEntitlement{MaxSandboxConcurrency: models.IntPtr(0)})

	req := mcpSSEConcurrencyRequest(uuid.NewString(), "any-token")
	rec := httptest.NewRecorder()
	mcpSseHandler(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402 for limit 0, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !containsString(rec.Body.String(), "creation not allowed") {
		t.Fatalf("expected 'creation not allowed' in body, got %s", rec.Body.String())
	}
}

func containsString(s, substr string) bool { return strings.Contains(s, substr) }
