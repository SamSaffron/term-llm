package upgrade

import (
	"github.com/spf13/cobra"
	"github.com/term-llm/term-llm/internal/update"
)

// Options contains the requested release version and the current build version.
type Options struct {
	Version        string
	CurrentVersion string
}

func (o Options) Run(cmd *cobra.Command, _ []string) error {
	return update.RunUpgrade(cmd.Context(), o.CurrentVersion, o.Version, cmd.OutOrStdout(), cmd.ErrOrStderr())
}
