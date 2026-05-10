package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print surfbot-cli version, commit, build date and platform",
	RunE: func(cmd *cobra.Command, args []string) error {
		_, err := fmt.Fprintf(
			cmd.OutOrStdout(),
			"surfbot-cli %s (commit %s, built %s, %s/%s, %s)\n",
			Version, Commit, BuildDate,
			runtime.GOOS, runtime.GOARCH, runtime.Version(),
		)
		return err
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
