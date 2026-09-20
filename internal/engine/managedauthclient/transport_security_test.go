package managedauthclient

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBrokerClientsRejectCredentialRedirects checks the actual constructor wiring for enrollment and connect requests.
func TestBrokerClientsRejectCredentialRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		attackerCalls := 0
		attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attackerCalls++
			w.WriteHeader(http.StatusOK)
		}))
		broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", attacker.URL)
			w.WriteHeader(status)
		}))
		client := NewHTTPBrokerClient(broker.URL, nil)
		_, _, _, err := client.Enroll(t.Context(), "fixture-ticket")
		require.Error(t, err)
		_, _, _, err = client.Refresh(t.Context(), "fixture-refresh", "fixture-ticket")
		require.Error(t, err)
		require.Error(t, client.Revoke(t.Context(), "fixture-refresh"))
		connect := NewConnectClient(broker.URL, nil, nil)
		req, err := http.NewRequest(http.MethodPost, broker.URL+"/exchange", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer fixture-installation")
		require.Error(t, connect.do(req, nil))
		require.Zero(t, attackerCalls)
		broker.Close()
		attacker.Close()
	}
}
