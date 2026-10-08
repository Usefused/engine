package mcpcatalog

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

var ErrEndpoint = errors.New("MCP requires a public HTTPS endpoint without credentials, a query, or a fragment")
var ErrDiscovery = errors.New("MCP discovery failed; check the endpoint, credentials, and supported transport")

// ValidateEndpoint keeps credentials out of stored URLs and restricts discovery to remote HTTPS servers.
func ValidateEndpoint(value string) error {
	parsed, err := url.Parse(value)
	// Query strings are excluded because they commonly carry provider credentials.
	if err != nil || len(value) > 4096 || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return ErrEndpoint
	}
	return nil
}

// publicAddress excludes local, reserved, metadata, and translation ranges before dialing the resolved address.
func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	// Global unicast alone still includes RFC1918 and several special-purpose ranges.
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, value := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20", "fec0::/10"} {
		// Special-purpose addresses must not bypass the egress boundary via public-looking DNS names.
		if netip.MustParsePrefix(value).Contains(ip) {
			return false
		}
	}
	return true
}

// publicDial resolves once and connects to the admitted IP, preventing a second DNS lookup from rebinding the target.
func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	// Only normal host/port pairs supplied by the HTTP transport are accepted.
	if err != nil {
		return nil, ErrEndpoint
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	// DNS failures are reported without exposing internal hostnames or resolver details.
	if err != nil || len(addresses) == 0 {
		return nil, ErrEndpoint
	}
	for _, ip := range addresses {
		// Reject mixed public/private answers instead of choosing a potentially unsafe fallback.
		if !publicAddress(ip) {
			return nil, ErrEndpoint
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
}

type boundedTransport struct {
	base     http.RoundTripper
	token    string
	endpoint string
	requests atomic.Int32
}

// RoundTrip binds a credential to the exact endpoint and limits every response, including SDK negotiation traffic.
func (t *boundedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Discovery has no reason to follow alternate URLs or issue unbounded background requests.
	if r.URL.String() != t.endpoint || t.requests.Add(1) > 220 {
		return nil, ErrDiscovery
	}
	req := r.Clone(r.Context())
	// A token is request-local and never shared across discovery clients or logged.
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	response, err := t.base.RoundTrip(req)
	// Provider errors can contain token-bearing URLs; callers receive a fixed safe error instead.
	if err != nil {
		return nil, ErrDiscovery
	}
	response.Body = http.MaxBytesReader(nil, response.Body, 2<<20)
	return response, nil
}

// discoveryHTTPClient owns an isolated transport with no environment proxy, cookies, redirects, or reusable credential state.
func discoveryHTTPClient(endpoint, token string) (*http.Client, func()) {
	transport := &http.Transport{DialContext: publicDial, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 32 << 10, DisableCompression: true}
	client := &http.Client{Timeout: 35 * time.Second, Transport: &boundedTransport{base: transport, token: token, endpoint: endpoint}, CheckRedirect: rejectRedirect}
	return client, transport.CloseIdleConnections
}

// rejectRedirect prevents both SSRF through redirect targets and credential forwarding to another endpoint.
func rejectRedirect(*http.Request, []*http.Request) error { return ErrEndpoint }

// ValidToken rejects control characters before adding Engine-owned credentials to an HTTP header.
func ValidToken(token string) bool {
	return token != "" && len(token) <= 16384 && !strings.ContainsAny(token, "\r\n\x00")
}
