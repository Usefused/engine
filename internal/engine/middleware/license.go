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
		if entitlement.EngineHeartbeatLeaseExpired.Load() && licenseRuntimeHTTPPath(r.URL.Path) {
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

func licenseRuntimeHTTPPath(path string) bool {
	return path == "/mcp/message" || path == "/mcp/call" || strings.HasPrefix(path, "/mcp/") || strings.HasPrefix(path, "/webhook/")
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
