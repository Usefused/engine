package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLicenseEnforcement_AllowsRequestsWhenNotSuspended(t *testing.T) {
	entitlement.EngineSuspended.Store(false)
	defer entitlement.EngineSuspended.Store(false)

	handler := LicenseEnforcement(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when not suspended, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestLicenseEnforcement_BlocksRequestsWhenSuspended(t *testing.T) {
	entitlement.EngineSuspended.Store(true)
	defer entitlement.EngineSuspended.Store(false)

	handler := LicenseEnforcement(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when suspended, got %d", rec.Code)
	}
	if rec.Body.String() != `{"error":"license suspended"}` {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %s", ct)
	}
}

func TestLicenseEnforcement_BlocksRequestsAfterHeartbeatGraceExpires(t *testing.T) {
	entitlement.LiveEntitlement.Reset()
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	entitlement.MarkHeartbeatSuccess(time.Now().Add(-2 * time.Minute))
	entitlement.EvaluateHeartbeatLease(time.Now(), time.Minute)
	handler := LicenseEnforcement(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/call", nil))

	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != `{"error":"license verification expired"}` {
		t.Fatalf("unexpected expired-license response: %d %s", rec.Code, rec.Body.String())
	}
}

// TestLicenseEnforcement_BlocksRESTExecutionAfterHeartbeatGraceExpires proves
// the direct app data plane observes the same runtime lease as SDK and MCP.
func TestLicenseEnforcement_BlocksRESTExecutionAfterHeartbeatGraceExpires(t *testing.T) {
	entitlement.LiveEntitlement.Reset()
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	entitlement.EngineHeartbeatLeaseExpired.Store(true)
	called := false
	handler := LicenseEnforcement(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/app-id/executions", nil))
	if rec.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("expired REST execution status=%d called=%t", rec.Code, called)
	}
}

// TestLicenseEnforcement_RESTExecutionMatcherIsExact proves route-like app
// control requests do not accidentally inherit the runtime heartbeat gate.
func TestLicenseEnforcement_RESTExecutionMatcherIsExact(t *testing.T) {
	entitlement.LiveEntitlement.Reset()
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	entitlement.EngineHeartbeatLeaseExpired.Store(true)
	handler := LicenseEnforcement(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	requests := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/v1/apps/app-id/executions"},
		{method: http.MethodPost, path: "/v1/apps/app-id/executions/"},
		{method: http.MethodPost, path: "/v1/apps/app-id/executions/retry"},
		{method: http.MethodPost, path: "/v1/apps//executions"},
	}
	for _, request := range requests {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(request.method, request.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s status=%d, want unchanged administrative handling", request.method, request.path, rec.Code)
		}
	}
}

func TestLicenseEnforcement_AllowsHealthAndAuthenticationDuringRestriction(t *testing.T) {
	entitlement.LiveEntitlement.Reset()
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	entitlement.EngineSuspended.Store(true)
	handler := LicenseEnforcement(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/health", "/auth/session", "/auth/license/exchange"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("recovery path %s returned %d", path, rec.Code)
		}
	}
}

func TestLicenseEnforcement_AllowsAdministrativeRoutesDuringHeartbeatOutage(t *testing.T) {
	entitlement.LiveEntitlement.Reset()
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	entitlement.EngineHeartbeatLeaseExpired.Store(true)
	handler := LicenseEnforcement(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/workspace/buckets/old", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("administrative route returned %d during heartbeat outage", rec.Code)
	}
}

func TestLicenseEnforcement_HeartbeatRecoveryClearsExpiredGate(t *testing.T) {
	entitlement.LiveEntitlement.Reset()
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	entitlement.MarkHeartbeatSuccess(time.Now().Add(-2 * time.Minute))
	entitlement.EvaluateHeartbeatLease(time.Now(), time.Minute)
	entitlement.MarkHeartbeatSuccess(time.Now())

	called := false
	_, err := UnaryLicenseEnforcement(context.Background(), nil, nil, func(context.Context, any) (any, error) {
		called = true
		return nil, nil
	})
	if err != nil || !called {
		t.Fatalf("successful heartbeat did not restore gRPC operations: called=%v err=%v", called, err)
	}
}

func TestUnaryLicenseEnforcement_BlocksExpiredLease(t *testing.T) {
	entitlement.LiveEntitlement.Reset()
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	entitlement.EngineHeartbeatLeaseExpired.Store(true)

	_, err := UnaryLicenseEnforcement(context.Background(), nil, nil, func(context.Context, any) (any, error) {
		t.Fatal("handler called while lease expired")
		return nil, nil
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("gRPC status = %v, want %v", status.Code(err), codes.Unavailable)
	}
}
