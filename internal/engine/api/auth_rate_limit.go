package api

import (
	"net/http"

	"github.com/Usefused/engine/internal/engine/browserauth"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

func limitAuthenticationRequests(limiter *browserauth.RequestLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter.Allow(r) {
				next.ServeHTTP(w, r)
				return
			}
			_, span := otel.Tracer("engine").Start(r.Context(), "engine.identity.request.rate_limited")
			span.SetAttributes(
				attribute.String("actor.type", "user"),
				attribute.String("identity.endpoint", r.URL.Path),
				attribute.String("outcome", "denied"),
			)
			span.End()
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "60")
			writeManagedLoginError(w, http.StatusTooManyRequests, "rate_limited")
		})
	}
}
