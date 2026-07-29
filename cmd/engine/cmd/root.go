package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	Version   = "dev"
	BuildHash = "dev"
)

var (
	ConfigPath string
)

// RootCmd is the base command for the engine CLI.
// We define a bare RootCmd without a `Run` block so that running `fused-engine`
// on its own prints the help menu rather than booting the server blindly.
// Commands like `start` or `version` are attached as children.
var RootCmd = &cobra.Command{
	Use:     "fused-engine",
	Version: Version,
	Short:   "Fused Engine - The data plane for executing generated SDKs.",
	Long: `Fused Engine is the high-performance data plane that securely proxies
and executes requests from generated SDKs and MCP servers.`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	// We make `--config` a PersistentFlag rather than a standard Flag so that it cascades
	// down to all future subcommands. Whether someone runs `start`, `migrate`, or `info`,
	// they should always be able to specify a custom config file path.
	RootCmd.PersistentFlags().StringVar(&ConfigPath, "config", "engine.yaml", "Path to configuration file")
}
