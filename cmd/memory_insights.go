package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
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
	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	agent := strings.TrimSpace(memoryAgent)
	insights, err := store.ListInsights(context.Background(), agent, insightLimit)
	if err != nil {
		return err
	}
	if len(insights) == 0 {
		fmt.Println("No insights found.")
		return nil
	}

	fmt.Printf("%-6s %-18s %-6s %-5s  %s\n", "ID", "CATEGORY", "CONF", "REINF", "COMPACT / CONTENT")
	fmt.Println(strings.Repeat("-", 80))
	for _, ins := range insights {
		cat := ins.Category
		if cat == "" {
			cat = "-"
		}
		// Show compact form if present, otherwise fall back to full content.
		preview := ins.CompactContent
		if preview == "" {
			preview = ins.Content
		}
		if len(preview) > 52 {
			preview = preview[:49] + "..."
		}
		preview = strings.ReplaceAll(preview, "\n", " ")
		fmt.Printf("%-6d %-18s %-6.2f %-5d  %s\n",
			ins.ID, truncateString(cat, 18), ins.Confidence, ins.ReinforcementCount, preview)
	}
	return nil
}

func runMemoryInsightsAdd(cmd *cobra.Command, args []string) error {
	agent := strings.TrimSpace(memoryAgent)
	if agent == "" {
		return fmt.Errorf("--agent is required")
	}
	content, err := readFragmentContent(insightContent)
	if err != nil {
		return err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("--content is required (or pipe content via stdin)")
	}

	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ins := &memorydb.Insight{
		Agent:          agent,
		Content:        content,
		CompactContent: strings.TrimSpace(insightCompact),
		Category:       strings.TrimSpace(insightCategory),
		TriggerDesc:    strings.TrimSpace(insightTrigger),
		Confidence:     insightConfidence,
	}
	if err := store.CreateInsight(context.Background(), ins); err != nil {
		return err
	}
	fmt.Printf("created insight: id=%d\n", ins.ID)
	return nil
}

func runMemoryInsightsUpdate(cmd *cobra.Command, args []string) error {
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("id must be an integer: %w", err)
	}
	content, err := readFragmentContent(insightContent)
	if err != nil {
		return err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("--content is required (or pipe content via stdin)")
	}

	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.UpdateInsight(context.Background(), id, content, strings.TrimSpace(insightCompact)); err != nil {
		return err
	}
	fmt.Printf("updated insight: id=%d\n", id)
	return nil
}

func runMemoryInsightsDelete(cmd *cobra.Command, args []string) error {
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("id must be an integer: %w", err)
	}

	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	deleted, err := store.DeleteInsight(context.Background(), id)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("insight not found: %d", id)
	}
	fmt.Printf("deleted insight: id=%d\n", id)
	return nil
}

func runMemoryInsightsReinforce(cmd *cobra.Command, args []string) error {
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("id must be an integer: %w", err)
	}

	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.ReinforceInsight(context.Background(), id); err != nil {
		return err
	}

	ins, _ := store.GetInsightByID(context.Background(), id)
	if ins != nil {
		fmt.Printf("reinforced insight %d: confidence=%.2f reinforcements=%d\n",
			id, ins.Confidence, ins.ReinforcementCount)
	} else {
		fmt.Printf("reinforced insight %d\n", id)
	}
	return nil
}

func runMemoryInsightsSearch(cmd *cobra.Command, args []string) error {
	query := strings.TrimSpace(args[0])
	agent := strings.TrimSpace(memoryAgent)

	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	insights, err := store.SearchInsights(context.Background(), agent, query, insightSearchLimit)
	if err != nil {
		return err
	}
	if len(insights) == 0 {
		fmt.Println("No matching insights found.")
		return nil
	}

	for _, ins := range insights {
		cat := ins.Category
		if cat == "" {
			cat = "uncategorized"
		}
		fmt.Printf("[%d] %s (conf=%.2f, reinf=%d)\n", ins.ID, cat, ins.Confidence, ins.ReinforcementCount)
		fmt.Println(ins.Content)
		fmt.Println()
	}
	return nil
}

func runMemoryInsightsExpand(cmd *cobra.Command, args []string) error {
	query := strings.TrimSpace(args[0])
	agent := strings.TrimSpace(memoryAgent)

	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	maxTok, _ := cmd.Flags().GetInt("max-tokens")
	if maxTok <= 0 {
		maxTok = 500
	}

	expanded, err := store.ExpandInsights(context.Background(), agent, query, maxTok)
	if err != nil {
		return err
	}
	if expanded == "" {
		fmt.Println("(no insights matched — bank may be empty or below confidence threshold)")
		return nil
	}
	fmt.Println(expanded)
	return nil
}

func runMemoryInsightsDecay(cmd *cobra.Command, args []string) error {
	agent := strings.TrimSpace(memoryAgent)

	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	halfLife, _ := cmd.Flags().GetFloat64("half-life")
	n, err := store.DecayInsights(context.Background(), agent, halfLife)
	if err != nil {
		return err
	}
	fmt.Printf("decayed %d insights (half-life=%.1f days)\n", n, halfLife)
	return nil
}

func runMemoryInsightsGC(cmd *cobra.Command, args []string) error {
	agent := strings.TrimSpace(memoryAgent)

	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	minConf, _ := cmd.Flags().GetFloat64("min-confidence")
	n, err := store.GCInsights(context.Background(), agent, minConf)
	if err != nil {
		return err
	}
	fmt.Printf("gc: deleted %d insights below confidence %.2f\n", n, minConf)
	return nil
}
