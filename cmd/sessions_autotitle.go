package cmd

import (
	"time"

	"github.com/spf13/cobra"
)

var sessionsAutotitleCmd = &cobra.Command{
	Use:   "autotitle",
	Short: "Generate candidate titles for recent sessions",
	Long: `Generate short and long candidate titles for recent sessions using the configured fast model.

Titles are saved by default. Use --dry-run to preview without saving.
User-set names always win in the UI and are not overwritten unless --force is provided.`,
	RunE: runSessionsAutotitle,
}

var (
	sessionsAutotitleDryRun  bool
	sessionsAutotitleForce   bool
	sessionsAutotitleMinAge  time.Duration
	sessionsAutotitleVerbose bool
)

func init() {
	sessionsAutotitleCmd.Flags().IntVar(&sessionsLimit, "limit", 50, "Maximum number of recent sessions to inspect")
	sessionsAutotitleCmd.Flags().BoolVar(&sessionsAutotitleDryRun, "dry-run", false, "Preview generated titles without saving")
	sessionsAutotitleCmd.Flags().BoolVar(&sessionsAutotitleForce, "force", false, "Regenerate even when a custom name already exists")
	sessionsAutotitleCmd.Flags().DurationVar(&sessionsAutotitleMinAge, "min-age", 3*time.Minute, "Skip sessions updated more recently than this duration")
	sessionsAutotitleCmd.Flags().BoolVarP(&sessionsAutotitleVerbose, "verbose", "v", false, "Print rejected candidates with rejection reason")
	sessionsCmd.AddCommand(sessionsAutotitleCmd)
}

func runSessionsAutotitle(cmd *cobra.Command, args []string) error {
	return sessionsApp().Autotitle(cmd, args)
}
