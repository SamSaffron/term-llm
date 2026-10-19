package cmd

import (
	"time"

	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/spf13/cobra"
)

var (
	memoryPromoteSince          time.Duration
	memoryPromoteRecentMaxBytes int
	memoryPromoteModel          string
)

var memoryPromoteCmd = &cobra.Command{
	Use:   "promote",
	Short: "Promote recently changed fragments into recent.md",
	RunE:  runMemoryPromote,
}

func init() {
	memoryPromoteCmd.Flags().DurationVar(&memoryPromoteSince, "since", 0, "Override promote lookback window (e.g. 6h)")
	memoryPromoteCmd.Flags().IntVar(&memoryPromoteRecentMaxBytes, "recent-max-bytes", appmemory.DefaultRecentMaxBytes, "Maximum bytes to keep in recent.md")
	memoryPromoteCmd.Flags().StringVar(&memoryPromoteModel, "model", "", "Override model used for promote")
}

func runMemoryPromote(cmd *cobra.Command, args []string) error {
	return appmemory.Promote(appmemory.PromoteOptions{
		Env:            memoryEnv(),
		Since:          memoryPromoteSince,
		RecentMaxBytes: memoryPromoteRecentMaxBytes,
		Model:          memoryPromoteModel,
	})
}
