package cmd

import (
	"github.com/samsaffron/term-llm/internal/update"
	"github.com/spf13/cobra"
)

var upgradeVersionFlag string

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade term-llm to the latest release",
	Long: `Upgrade term-llm to the latest release from GitHub.

By default, upgrades to the latest version. Use --version to install a specific version.

Examples:
  term-llm upgrade                    # upgrade to latest
  term-llm upgrade --version v0.2.0   # install specific version`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return update.RunUpgrade(cmd.Context(), Version, upgradeVersionFlag, cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

func init() {
	upgradeCmd.Flags().StringVar(&upgradeVersionFlag, "version", "", "Install a specific version (e.g. v0.2.0)")
	rootCmd.AddCommand(upgradeCmd)
}
