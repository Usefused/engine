package cmd

import (
	"github.com/Usefused/engine/internal/shared/config"
	"github.com/spf13/cobra"
)

// resolveListenerFlags applies only explicit CLI values after YAML and environment resolution.
func resolveListenerFlags(cmd *cobra.Command, server config.ServerConfig) (config.ServerConfig, error) {
	for name, destination := range map[string]*string{
		"port": &server.HTTPPort, "grpc-host": &server.GRPCHost,
		"grpc-port": &server.GRPCPort, "webhook-port": &server.WebhookPort,
	} {
		// Cobra defaults must never mask an operator's YAML or environment selection.
		if cmd.Flags().Changed(name) {
			value, err := cmd.Flags().GetString(name)
			// Flag type mismatches indicate invalid command wiring and must stop startup.
			if err != nil {
				return server, err
			}
			*destination = value
		}
	}
	return server, server.Validate()
}
