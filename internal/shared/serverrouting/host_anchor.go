package serverrouting

import (
	"net"
	"strings"
)

// markServerHostname retains actual enum/default routing while marking only
// unbounded app-owned hostname inputs for the registrable-domain check.
func markServerHostname(template string, variables []Variable, values map[string]string, supplied map[string]bool) (string, error) {
	hostname := templateHostname(template)
	markedValues := make(map[string]string, len(values))
	for name, value := range values {
		markedValues[name] = value
	}
	marked := false
	for _, match := range placeholderPattern.FindAllStringSubmatch(hostname, -1) {
		// Scheme, port, and provider-enum values must remain valid during URL parsing.
		if supplied[match[1]] {
			markedValues[match[1]] = suppliedHostMarker
			marked = true
		}
	}
	// A path-only input never changes the authority and needs no domain marker.
	if !marked {
		return "", nil
	}
	reference, _, err := ResolveReference(hostname, variables, markedValues)
	return reference, err
}

// templateHostname isolates the authority before URL parsing, which cannot
// directly parse OpenAPI scheme or port placeholders.
func templateHostname(template string) string {
	authority := template
	// Absolute and protocol-relative servers share the same hostname boundary.
	if index := strings.Index(authority, "://"); index >= 0 {
		authority = authority[index+3:]
	} else if strings.HasPrefix(authority, "//") {
		authority = strings.TrimPrefix(authority, "//")
	} else {
		// Relative operation paths inherit an already-validated service authority.
		return ""
	}
	authority, _, _ = strings.Cut(authority, "/")
	host, _, err := net.SplitHostPort(authority)
	// SplitHostPort accepts a template port while preserving bracketed IPv6 hosts.
	if err == nil {
		return host
	}
	return authority
}
