package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// These will be set by the linker during build
var Version = "dev"
var Commit = "unknown"
var Date = "unknown"

func versionString() string {
	return fmt.Sprintf("%s (commit: %s, built: %s)", Version, Commit, Date)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "term-llm version %s\n", versionString())
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
