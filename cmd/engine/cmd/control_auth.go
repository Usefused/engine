package cmd

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/browserauth"
)

var controlPlanePrefixes = []string{
	"/auth",
	"/graphql",
	"/integrations",
	"/sdks",
	"/apps",
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

var publicEngineRoutes = map[string]struct{}{
	"/health":                {},
	"/auth/managed/start":    {},
	"/auth/managed/poll":     {},
	"/auth/session":          {},
	"/auth/license/exchange": {},
	"/auth/api-key/exchange": {},
	"/auth/logout":           {},
	"/auth/cli/start":        {},
	"/auth/cli/poll":         {},
	"/auth/cli/approve":      {},
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

func controlActorMiddlewareWithAudit(authenticator *accesscontrol.Authenticator, recorder accesscontrol.AuditRecorder, cookieManagers ...*browserauth.CookieManager) func(http.Handler) http.Handler {
	cookies := firstCookieManager(cookieManagers)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isControlPlaneRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			auditPath := controlAuditRouteFamily(r.URL.Path)
			credential, source, credentialErr := browserauth.CredentialFromRequest(r, cookies)
			if credentialErr != nil {
				recordControlAudit(r.Context(), recorder, newControlAuditEvent(r, accesscontrol.Actor{}, "control.authenticate", auditPath, nil, accesscontrol.AuditDenied, http.StatusUnauthorized, "ambiguous_credentials"))
				accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrAuthenticationRequired)
				return
			}
			if authenticator == nil {
				recordControlAudit(r.Context(), recorder, newControlAuditEvent(r, accesscontrol.Actor{}, "control.authenticate", auditPath, nil, accesscontrol.AuditDenied, http.StatusUnauthorized, "authenticator_unavailable"))
				accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrAuthenticationRequired)
				return
			}

			started := time.Now()
			actor, err := authenticator.AuthenticateControlCredential(r.Context(), credential)
			w.Header().Add("Server-Timing", fmt.Sprintf("engine_authn;dur=%.3f", float64(time.Since(started).Microseconds())/1000))
			if err != nil {
				recordControlAudit(r.Context(), recorder, newControlAuditEvent(r, accesscontrol.Actor{}, "control.authenticate", auditPath, nil, accesscontrol.AuditDenied, http.StatusUnauthorized, "authentication_failed"))
				accesscontrol.WriteAuthorizationError(w, err)
				return
			}
			if denial := validateCookieAuthentication(r, cookies, actor, source, credential); denial != nil {
				recordControlAudit(r.Context(), recorder, newControlAuditEvent(r, actor, denial.action, auditPath, nil, accesscontrol.AuditDenied, denial.status, denial.reason))
				accesscontrol.WriteAuthorizationError(w, denial.err)
				return
			}
			authenticated := r.Clone(accesscontrol.ContextWithActor(r.Context(), actor))
			if source == browserauth.CredentialSourceCookie {
				// Existing handlers consume this internal header shape. Populating it only
				// after cookie authentication avoids a second authorization code path.
				authenticated.Header = r.Header.Clone()
				authenticated.Header.Set("X-API-Key", credential)
			}
			next.ServeHTTP(w, authenticated)
		})
	}
}

type cookieAuthenticationDenial struct {
	action string
	reason string
	status int
	err    error
}

func validateCookieAuthentication(r *http.Request, cookies *browserauth.CookieManager, actor accesscontrol.Actor, source browserauth.CredentialSource, credential string) *cookieAuthenticationDenial {
	if source != browserauth.CredentialSourceCookie {
		return nil
	}
	if !browserauth.IsBrowserSessionActor(actor) {
		return &cookieAuthenticationDenial{
			action: "control.authenticate", reason: "invalid_browser_credential",
			status: http.StatusUnauthorized, err: accesscontrol.ErrAuthenticationRequired,
		}
	}
	if browserauth.RequiresCSRF(r.Method) && !cookies.ValidateCSRF(r, credential) {
		return &cookieAuthenticationDenial{
			action: "control.csrf", reason: "csrf_denied",
			status: http.StatusForbidden, err: accesscontrol.ErrPolicyDenied,
		}
	}
	return nil
}

func firstCookieManager(managers []*browserauth.CookieManager) *browserauth.CookieManager {
	if len(managers) == 0 {
		return nil
	}
	return managers[0]
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
	_, publicRoute := publicEngineRoutes[r.URL.Path]
	if r.Method == http.MethodOptions || publicRoute {
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
