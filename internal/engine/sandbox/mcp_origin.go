package sandbox

import (
	"net/http"
	"net/url"
	"strings"
)

const mcpOriginForbiddenCode = "mcp_origin_forbidden"

// admitMCPRequestOrigin rejects browser cross-origin traffic before authentication or session lookup.
func admitMCPRequestOrigin(w http.ResponseWriter, r *http.Request) bool {
	// Non-browser MCP clients normally omit Origin and remain compatible with both transports.
	if mcpRequestOriginAllowed(r) {
		return true
	}
	writeError(w, http.StatusForbidden, mcpOriginForbiddenCode)
	return false
}

// mcpRequestOriginAllowed admits absent Origin or one exact HTTP(S) origin matching the request authority.
func mcpRequestOriginAllowed(r *http.Request) bool {
	values := r.Header.Values("Origin")
	// An absent header carries no browser-controlled origin to validate.
	if len(values) == 0 {
		return true
	}
	// MCP accepts one serialized browser origin; duplicates and empty values are ambiguous at proxy boundaries.
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return false
	}
	raw := strings.TrimSpace(values[0])
	origin, ok := parseMCPRequestOrigin(raw)
	// Malformed or URL-like values cannot be trusted as serialized browser origins.
	if !ok {
		return false
	}
	// Only browser network schemes can represent a same-origin MCP request.
	if !strings.EqualFold(origin.Scheme, "http") && !strings.EqualFold(origin.Scheme, "https") {
		return false
	}
	return strings.EqualFold(origin.Host, r.Host)
}

// parseMCPRequestOrigin accepts only the authority-shaped URL grammar emitted by browsers.
func parseMCPRequestOrigin(raw string) (*url.URL, bool) {
	origin, err := url.Parse(raw)
	// Parse failures, opaque values, and embedded credentials cannot identify a trustworthy browser authority.
	if err != nil || origin.Opaque != "" || origin.User != nil {
		return nil, false
	}
	// An Origin has an authority but no path, query, or fragment from a larger URL.
	if origin.Host == "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, false
	}
	return origin, true
}
