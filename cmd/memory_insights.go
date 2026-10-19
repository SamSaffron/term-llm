package cmd

import (
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/spf13/cobra"
)

var (
	insightCategory      string
	insightTrigger       string
	insightConfidence    float64
	insightContent       string
	insightCompact       string
	insightLimit         int
	insightSearchLimit   int
	insightHalfLifeDays  float64
	insightMinConfidence float64
)

var memoryInsightsCmd = &cobra.Command{
	Use:   "insights",
	Short: "Manage behavioral insight rules",
	Long: `Manage the insight bank — generalized behavioral rules extracted from past sessions.

Unlike fragments (which store facts), insights store actionable patterns that
change how the agent behaves in future similar situations. High-confidence
insights are automatically injected at the start of each session.

Examples:
  term-llm memory insights list --agent jarvis
  term-llm memory insights add --agent jarvis --content "..." --category anti-pattern
  term-llm memory insights search "code review" --agent jarvis
  term-llm memory insights reinforce 42
  term-llm memory insights delete 42`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var memoryInsightsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List insights, newest first",
	RunE:  runMemoryInsightsList,
}

var memoryInsightsAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a new insight",
	RunE:  runMemoryInsightsAdd,
}

var memoryInsightsUpdateCmd = &cobra.Command{
	Use:   "update <id>",
	Short: "Update insight content by ID",
	Args:  cobra.ExactArgs(1),
	RunE:  runMemoryInsightsUpdate,
}

var memoryInsightsDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete an insight by ID",
	Args:  cobra.ExactArgs(1),
	RunE:  runMemoryInsightsDelete,
}

var memoryInsightsReinforceCmd = &cobra.Command{
	Use:   "reinforce <id>",
	Short: "Reinforce an insight (bump confidence + count)",
	Args:  cobra.ExactArgs(1),
	RunE:  runMemoryInsightsReinforce,
}

var memoryInsightsSearchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search insights by BM25 relevance",
	Args:  cobra.ExactArgs(1),
	RunE:  runMemoryInsightsSearch,
}

var memoryInsightsExpandCmd = &cobra.Command{
	Use:   "expand <query>",
	Short: "Preview the insight block that would be injected for a given query",
	Args:  cobra.ExactArgs(1),
	RunE:  runMemoryInsightsExpand,
}

var memoryInsightsDecayCmd = &cobra.Command{
	Use:   "decay",
	Short: "Apply exponential confidence decay to stale insights",
	RunE:  runMemoryInsightsDecay,
}

var memoryInsightsGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Delete insights below minimum confidence threshold",
	RunE:  runMemoryInsightsGC,
}

func init() {
	memoryInsightsCmd.AddCommand(memoryInsightsListCmd)
	memoryInsightsCmd.AddCommand(memoryInsightsAddCmd)
	memoryInsightsCmd.AddCommand(memoryInsightsUpdateCmd)
	memoryInsightsCmd.AddCommand(memoryInsightsDeleteCmd)
	memoryInsightsCmd.AddCommand(memoryInsightsReinforceCmd)
	memoryInsightsCmd.AddCommand(memoryInsightsSearchCmd)
	memoryInsightsCmd.AddCommand(memoryInsightsExpandCmd)
	memoryInsightsCmd.AddCommand(memoryInsightsDecayCmd)
	memoryInsightsCmd.AddCommand(memoryInsightsGCCmd)

	memoryInsightsListCmd.Flags().IntVar(&insightLimit, "limit", 20, "Maximum insights to show (0 = all)")

	memoryInsightsAddCmd.Flags().StringVar(&insightContent, "content", "", "Insight rule text (required, or pipe via stdin)")
	memoryInsightsAddCmd.Flags().StringVar(&insightCompact, "compact", "", "Short ≤15-word version for token-efficient injection (optional)")
	memoryInsightsAddCmd.Flags().StringVar(&insightCategory, "category", "", "Category: anti-pattern | communication-style | domain-approach | workflow | anticipation")
	memoryInsightsAddCmd.Flags().StringVar(&insightTrigger, "trigger", "", "When this insight applies (optional description)")
	memoryInsightsAddCmd.Flags().Float64Var(&insightConfidence, "confidence", 0.5, "Initial confidence 0.0–1.0")

	memoryInsightsUpdateCmd.Flags().StringVar(&insightContent, "content", "", "New insight content (required, or pipe via stdin)")
	memoryInsightsUpdateCmd.Flags().StringVar(&insightCompact, "compact", "", "New compact form (leave empty to keep existing)")

	memoryInsightsSearchCmd.Flags().IntVar(&insightSearchLimit, "limit", 5, "Maximum results")
	memoryInsightsExpandCmd.Flags().IntVar(&insightSearchLimit, "max-tokens", 500, "Token budget for expansion")
	memoryInsightsDecayCmd.Flags().Float64Var(&insightHalfLifeDays, "half-life", 30.0, "Decay half-life in days")
	memoryInsightsGCCmd.Flags().Float64Var(&insightMinConfidence, "min-confidence", 0.1, "Delete insights below this confidence")
}

func runMemoryInsightsList(cmd *cobra.Command, args []string) error {
	return appmemory.InsightsList(appmemory.InsightsListOptions{Env: memoryEnv(), Limit: insightLimit})
}

func runMemoryInsightsAdd(cmd *cobra.Command, args []string) error {
	return appmemory.InsightsAdd(appmemory.InsightsAddOptions{
		Env:        memoryEnv(),
		Content:    insightContent,
		Compact:    insightCompact,
		Category:   insightCategory,
		Trigger:    insightTrigger,
		Confidence: insightConfidence,
	})
}

func runMemoryInsightsUpdate(cmd *cobra.Command, args []string) error {
	return appmemory.InsightsUpdate(appmemory.InsightsUpdateOptions{
		Env:     memoryEnv(),
		Content: insightContent,
		Compact: insightCompact,
	}, args)
}

func runMemoryInsightsDelete(cmd *cobra.Command, args []string) error {
	return appmemory.InsightsDelete(appmemory.InsightsDeleteOptions{Env: memoryEnv()}, args)
}

func runMemoryInsightsReinforce(cmd *cobra.Command, args []string) error {
	return appmemory.InsightsReinforce(appmemory.InsightsReinforceOptions{Env: memoryEnv()}, args)
}

func runMemoryInsightsSearch(cmd *cobra.Command, args []string) error {
	return appmemory.InsightsSearch(appmemory.InsightsSearchOptions{Env: memoryEnv(), Limit: insightSearchLimit}, args)
}

func runMemoryInsightsExpand(cmd *cobra.Command, args []string) error {
	maxTokens, _ := cmd.Flags().GetInt("max-tokens")
	return appmemory.InsightsExpand(appmemory.InsightsExpandOptions{Env: memoryEnv(), MaxTokens: maxTokens}, args)
}

func runMemoryInsightsDecay(cmd *cobra.Command, args []string) error {
	halfLife, _ := cmd.Flags().GetFloat64("half-life")
	return appmemory.InsightsDecay(appmemory.InsightsDecayOptions{Env: memoryEnv(), HalfLifeDays: halfLife})
}

func runMemoryInsightsGC(cmd *cobra.Command, args []string) error {
	minConf, _ := cmd.Flags().GetFloat64("min-confidence")
	return appmemory.InsightsGC(appmemory.InsightsGCOptions{Env: memoryEnv(), MinConfidence: minConf})
}
