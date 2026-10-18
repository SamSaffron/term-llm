package version

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Options contains linker-injected build metadata.
type Options struct {
	Version string
	Commit  string
	Date    string
}

func (o Options) String() string {
	return fmt.Sprintf("%s (commit: %s, built: %s)", o.Version, o.Commit, o.Date)
}

func (o Options) Run(cmd *cobra.Command, _ []string) {
	fmt.Fprintf(cmd.OutOrStdout(), "term-llm version %s\n", o.String())
}
