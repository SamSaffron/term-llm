package cmd

import (
	versionapp "github.com/samsaffron/term-llm/internal/app/version"
	"github.com/samsaffron/term-llm/internal/buildinfo"
	"github.com/spf13/cobra"
)

// These will be set by the linker during build
var Version = "dev"
var Commit = "unknown"
var Date = "unknown"

func versionString() string {
	return (versionapp.Options{Version: Version, Commit: Commit, Date: Date}).String()
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		(versionapp.Options{Version: Version, Commit: Commit, Date: Date}).Run(cmd, args)
	},
}

func init() {
	buildinfo.Version = Version
	rootCmd.AddCommand(versionCmd)
}
