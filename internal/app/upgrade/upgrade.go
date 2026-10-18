package upgrade

import (
	"github.com/samsaffron/term-llm/internal/update"
	"github.com/spf13/cobra"
)

// Options contains the requested release version and the current build version.
type Options struct {
	Version        string
	CurrentVersion string
}

func (o Options) Run(cmd *cobra.Command, _ []string) error {
	return update.RunUpgrade(cmd.Context(), o.CurrentVersion, o.Version, cmd.OutOrStdout(), cmd.ErrOrStderr())
}
