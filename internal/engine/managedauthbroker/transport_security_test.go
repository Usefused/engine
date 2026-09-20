package managedauthbroker

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRegistryVerifierRejectsRedirects prevents a redirected ticket from authorizing an attacker-supplied identity.
func TestRegistryVerifierRejectsRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		calls := 0
		attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
		}))
		registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", attacker.URL)
			w.WriteHeader(status)
		}))
		_, err := NewRegistryTicketVerifier(registry.URL, nil).VerifyTicket(t.Context(), "fixture-ticket")
		require.Error(t, err)
		require.Zero(t, calls)
		registry.Close()
		attacker.Close()
	}
}
