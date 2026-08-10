package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMCPSessionAuthContextBindsConnectedUserOutsideToolArguments(t *testing.T) {
	resourceID := uuid.NewString()
	headers := http.Header{}
	headers.Set("X-Fused-End-User-Ref", "customer-42")
	headers.Set("X-Fused-Resource-ID", resourceID)

	context, err := mcpSessionAuthContext(headers)
	if err != nil {
		t.Fatalf("mcpSessionAuthContext() error = %v", err)
	}
	if context["fused_end_user_ref"] != "customer-42" || context["fused_resource_id"] != resourceID {
		t.Fatalf("unexpected auth context: %#v", context)
	}
}

func TestMCPSessionContextEndsAtTokenExpiry(t *testing.T) {
	expiresAt := time.Now().Add(20 * time.Millisecond)
	ctx, cancel := mcpSessionContext(context.Background(), &expiresAt)
	defer cancel()

	select {
	case <-ctx.Done():
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("session context error = %v, want deadline exceeded", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("session remained active after token expiry")
	}
}

func TestMCPSessionAuthContextRejectsInvalidResourceID(t *testing.T) {
	headers := http.Header{"X-Fused-Resource-Id": []string{"not-a-uuid"}}
	if _, err := mcpSessionAuthContext(headers); err == nil {
		t.Fatal("expected invalid resource ID to be rejected")
	}
}

func TestConnectMCPAppDoesNotForwardExecutionTokenToCache(t *testing.T) {
	originalCache, originalValidator := globalObjectCache, globalTokenValidator
	t.Cleanup(func() {
		globalObjectCache, globalTokenValidator = originalCache, originalValidator
	})
	cache := &recordingCache{}
	globalObjectCache = cache
	globalTokenValidator = &mockTokenValidator{validToken: "family-token", accountID: uuid.New()}

	recorder := httptest.NewRecorder()
	appID := uuid.NewString()
	ctx := context.Background()
	_, connected := connectMCPApp(recorder, ctx, appID, "family-token")
	if !connected {
		t.Fatalf("connectMCPApp failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if cache.connectedID != appID {
		t.Fatalf("connected app = %q, want %q", cache.connectedID, appID)
	}
	if cache.connectedContext != ctx {
		t.Fatal("MCP connect derived a cache context instead of forwarding the credential-free request context")
	}
}
