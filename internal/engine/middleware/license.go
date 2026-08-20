package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// LicenseEnforcement blocks all Engine HTTP requests when the account has been
// suspended by the Registry. It reads an atomic boolean set by the heartbeat
// goroutine so the check is lock-free and imposes negligible latency.
func LicenseEnforcement(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if licenseRecoveryHTTPPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if entitlement.EngineSuspended.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"license suspended"}`))
			return
		}
		if entitlement.EngineHeartbeatLeaseExpired.Load() && licenseRuntimeHTTPRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"license verification expired"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func licenseRecoveryHTTPPath(path string) bool {
	return path == "/health" || path == "/auth" || strings.HasPrefix(path, "/auth/")
}

// licenseRuntimeHTTPRequest identifies data-plane HTTP calls whose execution
// must stop when the Registry heartbeat lease is no longer valid.
func licenseRuntimeHTTPRequest(request *http.Request) bool {
	path := request.URL.Path
	if path == "/mcp/message" || path == "/mcp/call" || strings.HasPrefix(path, "/mcp/") || strings.HasPrefix(path, "/webhook/") {
		return true
	}
	if request.Method != http.MethodPost || !strings.HasPrefix(path, "/v1/apps/") {
		return false
	}
	// Match the mounted chi route exactly so administrative methods, trailing
	// slashes, and future app subresources retain their own license policy.
	parts := strings.Split(strings.TrimPrefix(path, "/v1/apps/"), "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] == "executions"
}

// UnaryLicenseEnforcement and StreamLicenseEnforcement apply the same runtime
// gate to SDK gRPC operations. Heartbeat recovery is outbound HTTP and is not
// affected by these interceptors.
func UnaryLicenseEnforcement(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := grpcLicenseError(); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func StreamLicenseEnforcement(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := grpcLicenseError(); err != nil {
		return err
	}
	return handler(srv, licenseEnforcedServerStream{ServerStream: stream})
}

type licenseEnforcedServerStream struct {
	grpc.ServerStream
}

func (s licenseEnforcedServerStream) SendMsg(message any) error {
	if err := grpcLicenseError(); err != nil {
		return err
	}
	return s.ServerStream.SendMsg(message)
}

func (s licenseEnforcedServerStream) RecvMsg(message any) error {
	if err := grpcLicenseError(); err != nil {
		return err
	}
	return s.ServerStream.RecvMsg(message)
}

func grpcLicenseError() error {
	if entitlement.EngineSuspended.Load() {
		return status.Error(codes.PermissionDenied, "license suspended")
	}
	if entitlement.EngineHeartbeatLeaseExpired.Load() {
		return status.Error(codes.Unavailable, "license verification expired")
	}
	return nil
}
