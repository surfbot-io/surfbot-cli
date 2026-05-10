// Package cli wires up the surfbot-cli cobra command tree.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Build-time metadata, set from cmd/surfbot-cli/main.go via ldflags.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

var rootCmd = &cobra.Command{
	Use:   "surfbot-cli",
	Short: "Cloud-bridge endpoint for Surfbot",
	Long: `surfbot-cli is the on-host endpoint for the Surfbot cloud platform.

It maintains a persistent WebSocket connection to api.surfbot.io and
executes whatever the cloud dispatches inside a local sandbox. It is
not a standalone scanner: detection logic, scheduling, and findings
storage live in the cloud (see ADR-004 and SPEC-CLI1).`,
	SilenceUsage: true,
}

// Execute runs the root command. Exits with code 1 on error.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
