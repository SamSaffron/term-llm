package cmd

import (
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/spf13/cobra"
)

var (
	memoryUpdateRecentMaxInputChars int
	memoryUpdateRecentTargetTokens  int
	memoryUpdateRecentFragmentChars int
	memoryUpdateRecentModel         string
	memoryUpdateRecentFile          string
)

var memoryUpdateRecentCmd = &cobra.Command{
	Use:   "update-recent",
	Short: "Update recent.md with terse summaries of new session activity",
	RunE:  runMemoryUpdateRecent,
}

func init() {
	memoryUpdateRecentCmd.Flags().IntVar(&memoryUpdateRecentMaxInputChars, "max-input-chars", appmemory.DefaultUpdateRecentMaxInputChars, "Max chars of session text to include in the prompt")
	memoryUpdateRecentCmd.Flags().IntVar(&memoryUpdateRecentTargetTokens, "target-recent-tokens", appmemory.DefaultUpdateRecentTargetTokens, "Target size of recent.md in tokens (~4 chars/token); high water mark is +20%")
	memoryUpdateRecentCmd.Flags().StringVar(&memoryUpdateRecentModel, "model", "", "Override model used for update-recent")
	memoryUpdateRecentCmd.Flags().StringVarP(&memoryUpdateRecentFile, "file", "f", "", "Path to recent.md file (overrides default agent path)")
	memoryUpdateRecentCmd.Flags().IntVar(&memoryUpdateRecentFragmentChars, "fragment-chars", appmemory.DefaultUpdateRecentFragmentChars, "Max chars of recent memory fragments to include in the prompt")
}

func runMemoryUpdateRecent(cmd *cobra.Command, args []string) error {
	return appmemory.UpdateRecent(appmemory.UpdateRecentOptions{
		Env:           memoryEnv(),
		MaxInputChars: memoryUpdateRecentMaxInputChars,
		TargetTokens:  memoryUpdateRecentTargetTokens,
		FragmentChars: memoryUpdateRecentFragmentChars,
		Model:         memoryUpdateRecentModel,
		File:          memoryUpdateRecentFile,
	})
}
