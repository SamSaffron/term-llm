package cmd

import (
	upgradeapp "github.com/samsaffron/term-llm/internal/app/upgrade"
	"github.com/spf13/cobra"
)

var upgradeOptions upgradeapp.Options

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade term-llm to the latest release",
	Long: `Upgrade term-llm to the latest release from GitHub.

By default, upgrades to the latest version. Use --version to install a specific version.

Examples:
  term-llm upgrade                    # upgrade to latest
  term-llm upgrade --version v0.2.0   # install specific version

An executable owned by a desktop installer (a "<executable>.managed.json"
sidecar beside the resolved binary) refuses self-upgrade. Update it through that
installer instead.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		options := upgradeOptions
		options.CurrentVersion = Version
		return options.Run(cmd, args)
	},
}

func init() {
	upgradeCmd.Flags().StringVar(&upgradeOptions.Version, "version", "", "Install a specific version (e.g. v0.2.0)")
	rootCmd.AddCommand(upgradeCmd)
}
