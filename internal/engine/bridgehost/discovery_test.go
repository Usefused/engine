package bridgehost

import (
	"github.com/stretchr/testify/require"
	"testing"
)

// TestBridgeDiscoveryOrigins prevents incomplete or cleartext remote endpoints from receiving installation credentials.
func TestBridgeDiscoveryOrigins(t *testing.T) {
	for _, address := range []string{"https://engine.example", "grpcs://engine.example:443", "http://127.0.0.1:50051", "grpc://[::1]:50051"} {
		require.True(t, validBridgeEndpoint(address, true), address)
	}
	for _, address := range []string{"", "http://engine.example", "https://user:password@engine.example", "https://engine.example/path", "https://engine.example?token=x", "file:///tmp/engine"} {
		require.False(t, validBridgeEndpoint(address, true), address)
	}
	require.True(t, validBridgeEndpoint("https://engine.example/prefix", false))
	require.False(t, validBridgeEndpoint("grpcs://engine.example", false))
}
