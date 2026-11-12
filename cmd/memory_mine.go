package cmd

import (
	"fmt"
	"time"

	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/spf13/cobra"
)

var (
	memoryMineModel             string
	memoryMineSince             time.Duration
	memoryMineMessageSince      string
	memoryMineMessageBefore     string
	memoryMineLimit             int
	memoryMineBatchSize         int
	memoryMineIncludeSubagents  bool
	memoryMineMaxMessages       int
	memoryMineReadBytes         int
	memoryMineEmbed             bool
	memoryMineEmbedProvider     string
	memoryMinePromote           string
	memoryMinePromoteEvery      time.Duration
	memoryMineHalfLifeDays      float64
	memoryMineInsights          bool
	memoryMinePromptMaxTokens   int
	memoryMineTaxonomyMaxTokens int
	memoryMineToolMaxTurns      int
	memoryMineMaxOutputTokens   int
	memoryMineShowStats         bool
	memoryMineUseFastModel      bool
)

var memoryMineCmd = &cobra.Command{
	Use:   "mine",
	Short: "Mine sessions into memory fragments",
	RunE:  runMemoryMine,
}

func init() {
	memoryMineCmd.Flags().StringVar(&memoryMineModel, "model", "", "Override model used for memory extraction")
	memoryMineCmd.Flags().DurationVar(&memoryMineSince, "since", 0, "Only mine sessions updated within this duration (e.g. 24h)")
	memoryMineCmd.Flags().StringVar(&memoryMineMessageSince, "message-since", "", "Only mine messages at or after this RFC3339 timestamp (inclusive; never clips evidence)")
	memoryMineCmd.Flags().StringVar(&memoryMineMessageBefore, "message-before", "", "Stop before this RFC3339 timestamp (exclusive; retain boundary and later sequences)")
	memoryMineCmd.Flags().IntVar(&memoryMineLimit, "limit", 0, "Maximum number of extraction attempts (0 = all)")
	memoryMineCmd.Flags().IntVar(&memoryMineBatchSize, "batch-size", 10, "Number of messages to fetch per pagination request")
	memoryMineCmd.Flags().BoolVar(&memoryMineIncludeSubagents, "include-subagents", false, "Include subagent sessions")
	memoryMineCmd.Flags().IntVar(&memoryMineMaxMessages, "max-messages", 0, "Maximum newly mined messages per session (0 = all)")
	memoryMineCmd.Flags().IntVar(&memoryMineReadBytes, "read-bytes", 2048, "Maximum bytes returned by fragment lookup tools during mining")
	memoryMineCmd.Flags().BoolVar(&memoryMineEmbed, "embed", true, "Embed new/updated fragments after mining")
	memoryMineCmd.Flags().StringVar(&memoryMineEmbedProvider, "embed-provider", "", "Override embedding provider used in EMBED phase (optionally provider:model)")
	memoryMineCmd.Flags().StringVar(&memoryMinePromote, "promote", "never", "Promotion mode: auto|always|never")
	memoryMineCmd.Flags().DurationVar(&memoryMinePromoteEvery, "promote-every", 6*time.Hour, "Minimum interval between auto-promote runs")
	memoryMineCmd.Flags().Float64Var(&memoryMineHalfLifeDays, "half-life", 30.0, "Deprecated no-op; decay is no longer applied during memory mine")
	_ = memoryMineCmd.Flags().MarkDeprecated("half-life", "decay is no longer applied during memory mine; use explicit maintenance commands if needed")
	memoryMineCmd.Flags().BoolVar(&memoryMineInsights, "insights", false, "Also run insight extraction pass after fragment mining")
	memoryMineCmd.Flags().IntVar(&memoryMinePromptMaxTokens, "prompt-max-tokens", appmemory.DefaultMinePromptMaxTokens, "Approximate maximum prompt budget per extraction request")
	memoryMineCmd.Flags().IntVar(&memoryMineTaxonomyMaxTokens, "taxonomy-max-tokens", appmemory.DefaultMineTaxonomyMaxTokens, "Approximate maximum tokens for the compact fragment taxonomy map")
	memoryMineCmd.Flags().IntVar(&memoryMineToolMaxTurns, "tool-max-turns", appmemory.DefaultMineToolMaxTurns, "Maximum tool-assisted turns allowed for one extraction request")
	memoryMineCmd.Flags().IntVar(&memoryMineMaxOutputTokens, "max-output-tokens", appmemory.DefaultMineMaxOutputTokens, "Maximum output tokens for one extraction request")
	memoryMineCmd.Flags().BoolVar(&memoryMineShowStats, "stats", false, "Print prompt, tool, token, and timing stats for each mined batch")
	memoryMineCmd.Flags().BoolVar(&memoryMineUseFastModel, "fast-model", false, "Use the configured fast provider/model for memory extraction")
	memoryMineCmd.RegisterFlagCompletionFunc("embed-provider", EmbedProviderFlagCompletion)
}

func runMemoryMine(cmd *cobra.Command, args []string) error {
	since, err := parseMemoryMessageTime("message-since", memoryMineMessageSince)
	if err != nil {
		return err
	}
	before, err := parseMemoryMessageTime("message-before", memoryMineMessageBefore)
	if err != nil {
		return err
	}
	return appmemory.Mine(appmemory.MineOptions{
		MessageSince: since, MessageBefore: before,
		Env:               memoryEnv(),
		Model:             memoryMineModel,
		Since:             memoryMineSince,
		Limit:             memoryMineLimit,
		BatchSize:         memoryMineBatchSize,
		IncludeSubagents:  memoryMineIncludeSubagents,
		MaxMessages:       memoryMineMaxMessages,
		ReadBytes:         memoryMineReadBytes,
		Embed:             memoryMineEmbed,
		EmbedProvider:     memoryMineEmbedProvider,
		Promote:           memoryMinePromote,
		PromoteEvery:      memoryMinePromoteEvery,
		Insights:          memoryMineInsights,
		PromptMaxTokens:   memoryMinePromptMaxTokens,
		TaxonomyMaxTokens: memoryMineTaxonomyMaxTokens,
		ToolMaxTurns:      memoryMineToolMaxTurns,
		MaxOutputTokens:   memoryMineMaxOutputTokens,
		ShowStats:         memoryMineShowStats,
		UseFastModel:      memoryMineUseFastModel,
	})
}

func parseMemoryMessageTime(flag, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || t.IsZero() {
		return time.Time{}, fmt.Errorf("--%s must be a nonzero RFC3339 timestamp with timezone", flag)
	}
	return t, nil
}
