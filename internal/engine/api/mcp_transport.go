package api

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

const mcpDefaultTransport = "streamable_http"

// mcpTransportURLs keeps stable upgrade discovery and immutable pinning in one
// typed projection. SSE remains available only for older clients.
type mcpTransportURLs struct {
	StreamableHTTP          string `json:"streamable_http"`
	SSE                     string `json:"sse"`
	VersionedStreamableHTTP string `json:"versioned_streamable_http"`
	VersionedSSE            string `json:"versioned_sse"`
}

// mcpTransportURLsForApp advertises the family URL as the upgrade-safe default
// while retaining exact version URLs for controlled pinning and rollback.
func mcpTransportURLsForApp(r *http.Request, familyID, appID, stableAppID uuid.UUID) mcpTransportURLs {
	streamableHTTP, sse := "", ""
	// A family with no promoted version must not advertise a broken endpoint as
	// recommended merely because a pinned sibling remains runnable.
	if stableAppID != uuid.Nil {
		streamableHTTP = mcpStreamableHTTPURLForID(r, familyID)
		sse = streamableHTTP + "/sse"
	}
	versionedStreamableHTTP := mcpStreamableHTTPURLForID(r, appID)
	return mcpTransportURLs{
		StreamableHTTP:          streamableHTTP,
		SSE:                     sse,
		VersionedStreamableHTTP: versionedStreamableHTTP,
		VersionedSSE:            versionedStreamableHTTP + "/sse",
	}
}

// mcpStreamableHTTPURLForID returns a directly usable transport URL so
// authentication never depends on surviving an origin-changing redirect.
func mcpStreamableHTTPURLForID(r *http.Request, routeID uuid.UUID) string {
	path := "/mcp/" + routeID.String()
	// Request-free projections remain relative because no public authority is available.
	if r == nil {
		return path
	}
	scheme := mcpRequestScheme(r)
	host := mcpRequestHost(r)
	// Plain HTTP is safe only for the same-machine development origins that do
	// not redirect credentials across a public scheme boundary.
	if scheme == "http" && !mcpAllowsPlainHTTP(host) {
		scheme = "https"
	}
	return scheme + "://" + host + path
}

// mcpAllowsPlainHTTP limits insecure discovery to explicit loopback hosts;
// private and public names must advertise the TLS endpoint clients can call directly.
func mcpAllowsPlainHTTP(authority string) bool {
	parsed, err := url.Parse("http://" + authority)
	// Invalid authorities are handled by the existing host validator and cannot opt into HTTP here.
	if err != nil {
		return false
	}
	hostname := parsed.Hostname()
	// The conventional loopback name supports local development without requiring certificates.
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	address := net.ParseIP(hostname)
	return address != nil && address.IsLoopback()
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

// mcpTransportURLsGraphQLValue keeps REST and GraphQL transport field names identical.
func mcpTransportURLsGraphQLValue(urls mcpTransportURLs) map[string]interface{} {
	return map[string]interface{}{
		"streamable_http":           urls.StreamableHTTP,
		"sse":                       urls.SSE,
		"versioned_streamable_http": urls.VersionedStreamableHTTP,
		"versioned_sse":             urls.VersionedSSE,
	}
}
