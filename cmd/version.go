package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// These will be set by the linker during build
var Version = "dev"
var Commit = "unknown"
var Date = "unknown"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("term-llm version %s (commit: %s, built: %s)\n", Version, Commit, Date)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
