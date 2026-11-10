package api

import (
	"context"
	"net/http"
	"os"
)

type enginePublicURLContextKey struct{}

// EnginePublicURLMiddleware shares the loaded operator configuration with API projections, including headless deployments.
func EnginePublicURLMiddleware(publicURL string) func(http.Handler) http.Handler {
	// Capture configuration at startup; request Host and forwarding headers never define provider destinations.
	return func(next http.Handler) http.Handler {
		// Each request carries the same trusted URL that the embedded UI receives.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), enginePublicURLContextKey{}, publicURL)))
		})
	}
}

// enginePublicURL prefers loaded YAML/environment configuration over the legacy environment-only projection.
func enginePublicURL(ctx context.Context) string {
	// An explicitly empty loaded value remains authoritative, rather than falling back to request-derived origins.
	if value, ok := ctx.Value(enginePublicURLContextKey{}).(string); ok {
		return value
	}
	return os.Getenv("FUSED_ENGINE_PUBLIC_URL")
}
