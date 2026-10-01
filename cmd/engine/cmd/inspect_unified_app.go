package cmd

import (
	"errors"
	"io"

	"github.com/Usefused/engine/internal/engine/executionappvm"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/spf13/cobra"
)

// init exposes local declaration inspection through the same confinement as hosted planning.
func init() {
	RootCmd.AddCommand(&cobra.Command{
		Use: "inspect-unified-app-bundle", Short: "Inspect a Unified App bundle from stdin in the OS-confined worker",
		Args: cobra.NoArgs, SilenceUsage: true, RunE: inspectUnifiedAppBundle,
	})
}

// inspectUnifiedAppBundle accepts bounded source, with no Engine configuration, credentials, or host effects.
func inspectUnifiedAppBundle(command *cobra.Command, _ []string) error {
	bundle, err := io.ReadAll(io.LimitReader(command.InOrStdin(), executionappvm.MaxBundleBytes+1))
	// Reject oversized stdin before starting a worker process.
	if err != nil || len(bundle) == 0 || len(bundle) > executionappvm.MaxBundleBytes {
		return errors.New("invalid or oversized Unified App bundle")
	}
	manifest, err := sandbox.InspectCapabilityBundle(command.Context(), bundle)
	// Unsupported isolation never falls back to Node or in-process evaluation.
	if err != nil {
		return err
	}
	_, err = command.OutOrStdout().Write(manifest)
	return err
}
