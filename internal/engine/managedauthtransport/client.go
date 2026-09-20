// Package managedauthtransport confines delegation credentials to encrypted, non-redirecting requests.
package managedauthtransport

import (
	"errors"
	"net"
	"net/http"
	"time"
)

// New preserves caller TLS settings while applying one transport policy to every managed-auth credential hop.
func New(client *http.Client, timeout time.Duration) *http.Client {
	// A private copy prevents credential policy from changing an unrelated caller's client.
	if client == nil {
		client = http.DefaultClient
	}
	bounded := *client
	bounded.CheckRedirect = rejectRedirect
	// A caller's shorter deadline remains authoritative; unbounded requests are prohibited.
	if bounded.Timeout <= 0 || bounded.Timeout > timeout {
		bounded.Timeout = timeout
	}
	transport := bounded.Transport
	// Default TLS verification is preserved when no custom transport was supplied.
	if transport == nil {
		transport = http.DefaultTransport
	}
	bounded.Transport = credentialTransport{next: transport}
	return &bounded
}

type credentialTransport struct{ next http.RoundTripper }

// RoundTrip checks the destination before credentials can reach any configured transport.
func (t credentialTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u := r.URL
	// Reject ambiguous credential destinations rather than repairing a misconfigured endpoint.
	if u == nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return nil, errors.New("invalid managed-auth destination")
	}
	// Only literal loopback HTTP is allowed for local tests; DNS names cannot acquire this exception.
	if u.Scheme != "https" && !(u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback()) {
		return nil, errors.New("managed-auth destination requires HTTPS")
	}
	return t.next.RoundTrip(r)
}

// rejectRedirect prevents 307/308 responses from replaying credential-bearing bodies at another destination.
func rejectRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
