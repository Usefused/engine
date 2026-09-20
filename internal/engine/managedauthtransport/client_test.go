package managedauthtransport

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type testTransport func(*http.Request) (*http.Response, error)

// RoundTrip observes requests at the wire boundary without contacting a provider.
func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestCredentialRedirectsNeverReachSecondDestination covers body replay and same-origin bearer forwarding.
func TestCredentialRedirectsNeverReachSecondDestination(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, target := range []string{"https://attacker.example/collect", "https://broker.example/other", "http://broker.example/cleartext"} {
			calls := 0
			original := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, "https://broker.example/refresh", r.URL.String())
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})}
			client := New(original, 10*time.Second)
			req, err := http.NewRequest(http.MethodPost, "https://broker.example/refresh", strings.NewReader(`{"refresh_token":"fixture-secret"}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer fixture-installation")
			response, err := client.Do(req)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, status, response.StatusCode)
			require.Equal(t, 1, calls)
			require.Nil(t, original.CheckRedirect)
			require.Zero(t, original.Timeout)
		}
	}
}

// TestCredentialDestinationPolicy rejects cleartext and ambiguous endpoints before touching the transport.
func TestCredentialDestinationPolicy(t *testing.T) {
	for _, raw := range []string{"http://broker.example/", "http://localhost/", "https://user:pass@broker.example/", "https://broker.example/?destination=other", "https://broker.example/#fragment"} {
		client := New(&http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("unsafe destination reached transport")
			return nil, nil
		})}, time.Second)
		req, err := http.NewRequest(http.MethodPost, raw, strings.NewReader("fixture-credential"))
		require.NoError(t, err)
		_, err = client.Do(req)
		require.Error(t, err)
	}
	for _, raw := range []string{"https://broker.example/", "http://127.0.0.1/", "http://[::1]/"} {
		calls := 0
		client := New(&http.Client{Timeout: time.Second, Transport: testTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		})}, 10*time.Second)
		resp, err := client.Get(raw)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, 1, calls)
		require.Equal(t, time.Second, client.Timeout)
	}
}
