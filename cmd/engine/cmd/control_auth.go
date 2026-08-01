package cmd

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

var controlPlanePrefixes = []string{
	"/graphql",
	"/integrations",
	"/sdks",
	"/audit",
	"/account",
	"/credits",
	"/leads",
	"/workspace",
	"/config",
	"/sdk-config",
	"/mcp-config",
	"/webhook-config",
	"/engine/graphql",
}

type engineRequestClass string

const (
	requestClassPublic          engineRequestClass = "public"
	requestClassRuntimeExcluded engineRequestClass = "runtime_excluded"
	requestClassControl         engineRequestClass = "control"
	requestClassUnclassified    engineRequestClass = "unclassified"
)

func controlActorMiddleware(authenticator *accesscontrol.Authenticator) func(http.Handler) http.Handler {
	return controlActorMiddlewareWithAudit(authenticator, nil)
}

func controlActorMiddlewareWithAudit(authenticator *accesscontrol.Authenticator, recorder accesscontrol.AuditRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isControlPlaneRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			auditPath := controlAuditRouteFamily(r.URL.Path)
			if authenticator == nil {
				recordControlAudit(r.Context(), recorder, newControlAuditEvent(r, accesscontrol.Actor{}, "control.authenticate", auditPath, nil, accesscontrol.AuditDenied, http.StatusUnauthorized, "authenticator_unavailable"))
				accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrAuthenticationRequired)
				return
			}

			started := time.Now()
			actor, err := authenticator.AuthenticateControlCredential(r.Context(), strings.TrimSpace(r.Header.Get("X-API-Key")))
			w.Header().Add("Server-Timing", fmt.Sprintf("engine_authn;dur=%.3f", float64(time.Since(started).Microseconds())/1000))
			if err != nil {
				recordControlAudit(r.Context(), recorder, newControlAuditEvent(r, accesscontrol.Actor{}, "control.authenticate", auditPath, nil, accesscontrol.AuditDenied, http.StatusUnauthorized, "authentication_failed"))
				accesscontrol.WriteAuthorizationError(w, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(accesscontrol.ContextWithActor(r.Context(), actor)))
		})
	}
}

func controlAuditRouteFamily(path string) string {
	for _, prefix := range controlPlanePrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return prefix
		}
	}
	// Authentication runs before route policy resolution, so only a fixed
	// server-owned fallback is safe when no known control family matches.
	return "/control"
}

func isControlPlaneRequest(r *http.Request) bool {
	return classifyEngineRequest(r) == requestClassControl
}

func classifyEngineRequest(r *http.Request) engineRequestClass {
	// Browser preflight and health probes cannot carry workspace credentials and
	// must stay public; business endpoints are classified below instead.
	if r.Method == http.MethodOptions || r.URL.Path == "/health" {
		return requestClassPublic
	}
	// Runtime tokens and provider callbacks have their own authenticators. A
	// control-plane API key check here would conflate those independent trust
	// boundaries and break generated SDK/MCP traffic.
	if isRuntimeControlExclusion(r.URL.Path) || strings.HasPrefix(r.URL.Path, "/mcp/") || strings.HasPrefix(r.URL.Path, "/webhook/") {
		return requestClassRuntimeExcluded
	}
	for _, prefix := range controlPlanePrefixes {
		if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
			return requestClassControl
		}
	}
	// Unknown routes remain distinct from public routes so the coverage guard
	// can fail closed instead of accidentally treating a new endpoint as public.
	return requestClassUnclassified
}

func isRuntimeControlExclusion(path string) bool {
	return path == "/workspace/connect/callback"
}
