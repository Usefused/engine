package sandbox

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestManagedAuthTicketCarriesEngineIdentity verifies enrollment reuses the configured Registry identity headers.
func TestManagedAuthTicketCarriesEngineIdentity(t *testing.T) {
	t.Setenv("FUSED_ENV", "development")
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ticket":"fixture-single-use-ticket"}`))
	}))
	defer server.Close()
	installation, runtime := uuid.New(), uuid.New()
	client := NewHTTPRegistryClient(server.URL+"/graphql", "fixture-license")
	require.NoError(t, client.ConfigureEngineIdentity(installation, runtime))
	ticket, err := client.MintManagedAuthEnrollmentTicket(t.Context())
	require.NoError(t, err)
	require.Equal(t, "fixture-single-use-ticket", ticket)
	request := <-requests
	require.Equal(t, "/api/engine/managed-auth/enrollment-ticket", request.URL.Path)
	require.Equal(t, "Bearer fixture-license", request.Header.Get("Authorization"))
	require.Equal(t, installation.String(), request.Header.Get("X-Fused-Installation-ID"))
	require.Equal(t, runtime.String(), request.Header.Get("X-Fused-Runtime-Instance-ID"))
}
