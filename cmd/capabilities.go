package cmd

import versionapp "github.com/samsaffron/term-llm/internal/app/version"

func init() {
	rootCmd.AddCommand(versionapp.NewCapabilitiesCommand())
}
