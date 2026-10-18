package cmd

import (
	"time"

	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/spf13/cobra"
)

var (
	memoryConsolidateApply          bool
	memoryConsolidateSince          time.Duration
	memoryConsolidateRecentMaxBytes int
	memoryConsolidateModel          string
	memoryConsolidateHalfLife       float64
	memoryConsolidateDecayLimit     int
	memoryConsolidateSkipDecay      bool
)

var memoryConsolidateCmd = &cobra.Command{
	Use:   "consolidate",
	Short: "Safely consolidate changed memory into recent.md",
	Long: `Safely consolidate changed memory fragments into the agent's current-state
recent.md summary.

Consolidate is conservative: it defaults to dry-run mode, prints the proposed
recent.md rewrite, and shows a non-destructive decay preview. It never deletes
fragments and never rewrites stored decay scores. Pass --apply to write the
updated recent.md file.`,
	RunE: runMemoryConsolidate,
}

func init() {
	memoryConsolidateCmd.Flags().BoolVar(&memoryConsolidateApply, "apply", false, "Write the consolidated recent.md (default is dry-run preview)")
	memoryConsolidateCmd.Flags().DurationVar(&memoryConsolidateSince, "since", 0, "Override consolidate lookback window (e.g. 6h)")
	memoryConsolidateCmd.Flags().IntVar(&memoryConsolidateRecentMaxBytes, "recent-max-bytes", appmemory.DefaultRecentMaxBytes, "Maximum bytes to keep in recent.md")
	memoryConsolidateCmd.Flags().StringVar(&memoryConsolidateModel, "model", "", "Override model used for consolidation")
	memoryConsolidateCmd.Flags().Float64Var(&memoryConsolidateHalfLife, "half-life", memorydb.DefaultDecayHalfLifeDays, "Half-life in days for non-destructive decay preview")
	memoryConsolidateCmd.Flags().IntVar(&memoryConsolidateDecayLimit, "decay-limit", 10, "Maximum low-score fragments to list in decay preview (0 = none)")
	memoryConsolidateCmd.Flags().BoolVar(&memoryConsolidateSkipDecay, "no-decay-preview", false, "Skip the non-destructive decay preview")
}

func runMemoryConsolidate(cmd *cobra.Command, args []string) error {
	return appmemory.Consolidate(appmemory.ConsolidateOptions{
		Env:            memoryEnv(),
		Apply:          memoryConsolidateApply,
		Since:          memoryConsolidateSince,
		RecentMaxBytes: memoryConsolidateRecentMaxBytes,
		Model:          memoryConsolidateModel,
		HalfLife:       memoryConsolidateHalfLife,
		DecayLimit:     memoryConsolidateDecayLimit,
		SkipDecay:      memoryConsolidateSkipDecay,
	})
}
