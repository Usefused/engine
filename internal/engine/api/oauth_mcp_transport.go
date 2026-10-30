package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/go-chi/chi/v5/middleware"
)

type mcpOAuthAudienceStore interface {
	OAuthMCPResource(context.Context, string) (string, error)
}

// MCPAuthDiscovery advertises OAuth on existing MCP URLs and rejects tokens issued for another resource.
func MCPAuthDiscovery(publicURL string, source any) func(http.Handler) http.Handler {
	// Construct once so every request uses the configured public origin rather than request headers.
	return func(next http.Handler) http.Handler {
		// Runtime authentication is independent of the control-plane browser middleware.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Internal MCP bridges and OAuth protocol endpoints keep their existing authenticators.
			if !canonicalMCPPath(r.URL.Path) || publicURL == "" {
				next.ServeHTTP(w, r)
				return
			}
			writer := &mcpOAuthResponseWriter{WrapResponseWriter: middleware.NewWrapResponseWriter(w, r.ProtoMajor), challenge: oauthMCPChallenge(publicURL, r.URL.Path)}
			parts := strings.Fields(r.Header.Get("Authorization"))
			// Authentication discovery must work before protocol negotiation or JSON body parsing.
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				writeOAuthTokenError(writer, http.StatusUnauthorized, "invalid_token")
				return
			}
			// Management OAuth tokens cannot masquerade as MCP credentials, even for the same user.
			if strings.HasPrefix(parts[1], "foat_") && !mcpOAuthAudienceMatches(r.Context(), source, parts[1], strings.TrimRight(publicURL, "/")+r.URL.Path) {
				writeOAuthTokenError(writer, http.StatusUnauthorized, "invalid_token")
				return
			}
			next.ServeHTTP(writer, r)
		})
	}
}

// mcpOAuthAudienceMatches performs an uncached, hash-only lookup before the normal runtime token validator.
func mcpOAuthAudienceMatches(ctx context.Context, source any, token, resource string) bool {
	repository, ok := source.(mcpOAuthAudienceStore)
	// Missing metadata support cannot weaken audience validation.
	if !ok {
		return false
	}
	actual, err := repository.OAuthMCPResource(ctx, accesscontrol.HashControlCredential(token))
	return err == nil && actual == resource
}

type mcpOAuthResponseWriter struct {
	middleware.WrapResponseWriter
	challenge string
}

// WriteHeader adds resource discovery to all transport authentication failures, including revoked tokens.
func (w *mcpOAuthResponseWriter) WriteHeader(status int) {
	// Successful responses and authorization denials do not advertise a redundant sign-in challenge.
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", w.challenge)
	}
	w.WrapResponseWriter.WriteHeader(status)
}

// Unwrap preserves net/http response-controller streaming support through the challenge adapter.
func (w *mcpOAuthResponseWriter) Unwrap() http.ResponseWriter { return w.WrapResponseWriter }
