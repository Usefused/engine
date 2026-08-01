package cmd

import "testing"

func TestEngineGRPCListenAddressDefaultsToLoopback(t *testing.T) {
	for _, host := range []string{"", "   ", "127.0.0.1"} {
		if got := engineGRPCListenAddress(host, "50051"); got != "127.0.0.1:50051" {
			t.Fatalf("listen address for %q = %q", host, got)
		}
	}
}

func TestEngineGRPCListenAddressAllowsExplicitHost(t *testing.T) {
	if got := engineGRPCListenAddress("::1", "50051"); got != "[::1]:50051" {
		t.Fatalf("explicit listen address = %q", got)
	}
}
