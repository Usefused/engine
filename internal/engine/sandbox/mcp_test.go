package sandbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestMCPSessionAuthContextBindsConnectedUserOutsideToolArguments verifies selectors remain outside model-authored tools.
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

// TestMCPSessionContextHasNoAgeDeadline protects active sessions using non-expiring tokens.
func TestMCPSessionContextHasNoAgeDeadline(t *testing.T) {
	ctx, cancel := mcpSessionContext(context.Background(), nil)
	defer cancel()
	// Inactivity is owned by the sliding timer, never a fixed context deadline.
	if deadline, ok := ctx.Deadline(); ok {
		t.Fatalf("session acquired an age deadline: %s", deadline)
	}
	tokenExpiry := time.Now().Add(time.Hour)
	tokenCtx, cancelToken := mcpSessionContext(context.Background(), &tokenExpiry)
	defer cancelToken()
	// A token valid beyond five minutes must not gain a shorter session deadline.
	if deadline, ok := tokenCtx.Deadline(); !ok || !deadline.Equal(tokenExpiry) {
		t.Fatalf("session deadline = %s/%t, want token expiry %s", deadline, ok, tokenExpiry)
	}
}

// TestMCPStreamContextHonoursTokenExpiry keeps the active-session fix inside the authentication boundary.
func TestMCPStreamContextHonoursTokenExpiry(t *testing.T) {
	deadline := time.Now().Add(20 * time.Millisecond)
	lifecycleCtx, endSession := mcpSessionContext(context.Background(), &deadline)
	defer endSession()
	ctx, cancel := mcpSessionRequestContext(context.Background(), &mcpSession{lifecycleCtx: lifecycleCtx})
	defer cancel()
	select {
	case <-ctx.Done():
		// Session cancellation propagates to every stream even without client disconnection.
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("stream context error = %v", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("SSE stream outlived token expiry")
	}
}

// TestRecordMCPSessionIdleTimeoutEmitsOnlyBoundedFacts protects telemetry from session secrets.
func TestRecordMCPSessionIdleTimeoutEmitsOnlyBoundedFacts(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})

	recordMCPSessionIdleTimeout(&mcpSession{appID: "app-safe", sessionID: "secret-session", token: "secret-token", transport: "sse"})
	spans := exporter.GetSpans()
	// Cleanup must emit one bounded event, never a second execution receipt.
	if len(spans) != 1 {
		t.Fatalf("limit spans = %d, want 1", len(spans))
	}
	attributes := make(map[string]string, len(spans[0].Attributes))
	allowed := map[string]struct{}{
		"app.id": {}, "mcp.transport": {}, "mcp.limit.kind": {},
		"mcp.limit.unit": {}, "mcp.limit.maximum": {}, "outcome": {},
	}
	// Inspect every emitted value rather than relying solely on the expected keys.
	for _, item := range spans[0].Attributes {
		value := item.Value.Emit()
		// Secret values must never be derivable from an otherwise-safe limit event.
		if strings.Contains(value, "secret-session") || strings.Contains(value, "secret-token") {
			t.Fatalf("attribute %q leaked session material", item.Key)
		}
		// Exact allowlisting makes any future high-cardinality field an explicit review.
		if _, ok := allowed[string(item.Key)]; !ok {
			t.Fatalf("unexpected limit attribute %q", item.Key)
		}
		attributes[string(item.Key)] = value
	}
	// Idle cleanup remains distinguishable from authorization and per-call failures.
	if len(attributes) != 6 || attributes["mcp.limit.kind"] != "session_idle" || attributes["outcome"] != "rejected" {
		t.Fatalf("limit attributes = %#v", attributes)
	}
	// The span status carries only the stable code, never a raw context error.
	if spans[0].Status.Description != "mcp_session_idle_timeout" {
		t.Fatalf("limit status = %#v", spans[0].Status)
	}
}

// TestMCPSessionAuthContextRejectsInvalidResourceID keeps malformed routing out of session authority.
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
