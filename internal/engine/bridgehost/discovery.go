package bridgehost

import (
	"net"
	"net/url"
	"strings"
)

// validBridgeEndpoint rejects credentials, ambiguous URL controls and cleartext remote installation traffic.
func validBridgeEndpoint(raw string, grpc bool) bool {
	parsed, err := url.Parse(raw)
	// Only a concrete configured origin may be advertised to an installation.
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	// gRPC channels connect to an authority rather than a provider-controlled path.
	if grpc && strings.Trim(parsed.Path, "/") != "" {
		return false
	}
	return bridgeEndpointTransport(parsed, grpc)
}

// bridgeEndpointTransport permits unencrypted transport only for explicit local development.
func bridgeEndpointTransport(parsed *url.URL, grpc bool) bool {
	// HTTPS works for both protocol endpoints; grpcs is an explicit channel spelling.
	if parsed.Scheme == "https" || (grpc && parsed.Scheme == "grpcs") {
		return true
	}
	// Unknown schemes cannot downgrade a saved installation credential.
	if parsed.Scheme != "http" && !(grpc && parsed.Scheme == "grpc") {
		return false
	}
	address := net.ParseIP(parsed.Hostname())
	return parsed.Hostname() == "localhost" || (address != nil && address.IsLoopback())
}
