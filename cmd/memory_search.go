package cmd

import (
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/spf13/cobra"
)

var (
	memorySearchLimit             int
	memorySearchJSON              bool
	memorySearchEmbedProvider     string
	memorySearchNoDecay           bool
	memorySearchApplyDecay        bool
	memorySearchFreshness         bool
	memorySearchFreshnessHalfLife float64
	memorySearchBM25Only          bool
	memorySearchTouch             bool
)

var memorySearchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search memory fragments (hybrid BM25 + vector)",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runMemorySearch,
}

func init() {
	memorySearchCmd.Flags().IntVar(&memorySearchLimit, "limit", 6, "Maximum number of results")
	memorySearchCmd.Flags().BoolVar(&memorySearchJSON, "json", false, "Output as JSON")
	memorySearchCmd.Flags().StringVar(&memorySearchEmbedProvider, "embed-provider", "", "Override embedding provider for query embedding (optionally provider:model)")
	memorySearchCmd.Flags().BoolVar(&memorySearchApplyDecay, "decay", false, "Apply stored decay score multiplier (opt-in)")
	memorySearchCmd.Flags().BoolVar(&memorySearchFreshness, "freshness", false, "Apply non-persistent timestamp freshness multiplier (opt-in)")
	memorySearchCmd.Flags().BoolVar(&memorySearchFreshness, "recency", false, "Alias for --freshness")
	memorySearchCmd.Flags().Float64Var(&memorySearchFreshnessHalfLife, "freshness-half-life", memorydb.DefaultDecayHalfLifeDays, "Freshness half-life in days for --freshness/--recency")
	memorySearchCmd.Flags().BoolVar(&memorySearchNoDecay, "no-decay", false, "Deprecated no-op; decay is no longer applied by default")
	_ = memorySearchCmd.Flags().MarkDeprecated("no-decay", "decay is no longer applied by default; use --decay to opt in")
	memorySearchCmd.Flags().BoolVar(&memorySearchBM25Only, "bm25-only", false, "Force BM25-only retrieval")
	memorySearchCmd.Flags().BoolVar(&memorySearchTouch, "touch", false, "Record access metadata for returned fragments")
	memorySearchCmd.RegisterFlagCompletionFunc("embed-provider", EmbedProviderFlagCompletion)
}

func runMemorySearch(cmd *cobra.Command, args []string) error {
	return appmemory.Search(appmemory.SearchOptions{
		Env:               memoryEnv(),
		Limit:             memorySearchLimit,
		JSON:              memorySearchJSON,
		EmbedProvider:     memorySearchEmbedProvider,
		ApplyDecay:        memorySearchApplyDecay,
		Freshness:         memorySearchFreshness,
		FreshnessHalfLife: memorySearchFreshnessHalfLife,
		BM25Only:          memorySearchBM25Only,
		Touch:             memorySearchTouch,
	}, args)
}
