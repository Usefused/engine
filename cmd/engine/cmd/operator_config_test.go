package cmd

import (
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/spf13/cobra"
	"testing"
)

// TestListenerFlagPrecedence proves default flags cannot hide YAML and explicit flags can repair invalid YAML.
func TestListenerFlagPrecedence(t *testing.T) {
	cmd := &cobra.Command{}
	for _, name := range []string{"port", "grpc-host", "grpc-port", "webhook-port"} {
		cmd.Flags().String(name, "", "")
	}
	server := config.ServerConfig{HTTPPort: "9010", GRPCHost: "127.0.0.2", GRPCPort: "5010", WebhookPort: "9011"}
	got, err := resolveListenerFlags(cmd, server)
	// Untouched Cobra defaults must leave the resolved YAML/environment selection intact.
	if err != nil || got != server {
		t.Fatalf("default flags changed listeners: %+v %v", got, err)
	}
	// Explicit flags are the highest-precedence listener source.
	if err := cmd.Flags().Set("port", "9020"); err != nil {
		t.Fatal(err)
	}
	// An empty explicit webhook flag intentionally disables a configured dedicated listener.
	if err := cmd.Flags().Set("webhook-port", ""); err != nil {
		t.Fatal(err)
	}
	server.HTTPPort = "invalid"
	got, err = resolveListenerFlags(cmd, server)
	// Validation must use the effective values, not an overridden lower-precedence value.
	if err != nil || got.HTTPPort != "9020" || got.WebhookPort != "" {
		t.Fatalf("explicit flags did not win: %+v %v", got, err)
	}
	server.GRPCPort = "65536"
	_, err = resolveListenerFlags(cmd, server)
	// Unoverridden invalid ports must fail before listening.
	if err == nil {
		t.Fatal("accepted an invalid gRPC port")
	}
}
