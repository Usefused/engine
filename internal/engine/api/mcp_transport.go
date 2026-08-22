package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

const mcpDefaultTransport = "streamable_http"

// mcpTransportURLs keeps transport discovery separate from runtime identity.
// Both URLs point at the same immutable app version; SSE is exposed only so
// older clients can connect without making it look equivalent to the default.
type mcpTransportURLs struct {
	StreamableHTTP string `json:"streamable_http"`
	SSE            string `json:"sse"`
}

func mcpTransportURLsForApp(r *http.Request, appID uuid.UUID) mcpTransportURLs {
	streamableHTTP := mcpStreamableHTTPURLForApp(r, appID)
	return mcpTransportURLs{
		StreamableHTTP: streamableHTTP,
		SSE:            streamableHTTP + "/sse",
	}
}

func mcpStreamableHTTPURLForApp(r *http.Request, appID uuid.UUID) string {
	path := "/mcp/" + appID.String()
	if r == nil {
		return path
	}
	scheme := mcpRequestScheme(r)
	host := mcpRequestHost(r)
	return scheme + "://" + host + path
}

func mcpRequestScheme(r *http.Request) string {
	// A trusted proxy can append multiple hops. The first value is the public
	// client-facing scheme; invalid values fall back to the direct connection.
	forwarded := firstForwardedValue(r.Header.Get("X-Forwarded-Proto"))
	if forwarded == "http" || forwarded == "https" {
		return forwarded
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func mcpRequestHost(r *http.Request) string {
	forwarded := firstForwardedValue(r.Header.Get("X-Forwarded-Host"))
	if validMCPURLAuthority(forwarded) {
		return forwarded
	}
	return r.Host
}

func firstForwardedValue(value string) string {
	value, _, _ = strings.Cut(value, ",")
	return strings.TrimSpace(value)
}

func validMCPURLAuthority(authority string) bool {
	if authority == "" || strings.ContainsAny(authority, "/?#@\\") {
		return false
	}
	parsed, err := url.Parse("http://" + authority)
	return err == nil && parsed.Host == authority && parsed.Hostname() != ""
}

func mcpTransportURLsGraphQLValue(urls mcpTransportURLs) map[string]interface{} {
	return map[string]interface{}{
		"streamable_http": urls.StreamableHTTP,
		"sse":             urls.SSE,
	}
}
