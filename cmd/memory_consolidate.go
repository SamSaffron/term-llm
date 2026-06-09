package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
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
	memoryConsolidateCmd.Flags().IntVar(&memoryConsolidateRecentMaxBytes, "recent-max-bytes", defaultRecentMaxBytes, "Maximum bytes to keep in recent.md")
	memoryConsolidateCmd.Flags().StringVar(&memoryConsolidateModel, "model", "", "Override model used for consolidation")
	memoryConsolidateCmd.Flags().Float64Var(&memoryConsolidateHalfLife, "half-life", memorydb.DefaultDecayHalfLifeDays, "Half-life in days for non-destructive decay preview")
	memoryConsolidateCmd.Flags().IntVar(&memoryConsolidateDecayLimit, "decay-limit", 10, "Maximum low-score fragments to list in decay preview (0 = none)")
	memoryConsolidateCmd.Flags().BoolVar(&memoryConsolidateSkipDecay, "no-decay-preview", false, "Skip the non-destructive decay preview")
}

func runMemoryConsolidate(cmd *cobra.Command, args []string) error {
	if memoryConsolidateRecentMaxBytes <= 0 {
		return fmt.Errorf("--recent-max-bytes must be > 0")
	}
	if memoryConsolidateHalfLife <= 0 {
		return fmt.Errorf("--half-life must be > 0")
	}
	if memoryConsolidateDecayLimit < 0 {
		return fmt.Errorf("--decay-limit must be >= 0")
	}

	apply, err := memoryConsolidateApplyMode(memoryDryRun, memoryConsolidateApply)
	if err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := applyProviderOverridesWithAgent(cfg, cfg.Ask.Provider, cfg.Ask.Model, "", "", ""); err != nil {
		return err
	}
	if strings.TrimSpace(memoryConsolidateModel) != "" {
		cfg.ApplyOverrides("", strings.TrimSpace(memoryConsolidateModel))
	}

	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}
	engine := newEngine(provider, cfg)

	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	agentName := strings.TrimSpace(memoryAgent)
	if agentName == "" {
		agentName = resolveMemoryAgent("")
	}

	if !apply {
		fmt.Println("consolidate: dry-run (no writes). Proposed recent.md follows; rerun with --apply to write it.")
	}

	promoted, err := runMemoryPromoteFlow(ctx, cfg, engine, store, memoryPromoteOptions{
		Agent:          agentName,
		Since:          memoryConsolidateSince,
		RecentMaxBytes: memoryConsolidateRecentMaxBytes,
		Model:          strings.TrimSpace(memoryConsolidateModel),
		DryRun:         !apply,
	})
	if err != nil {
		return err
	}

	if !memoryConsolidateSkipDecay {
		if err := printMemoryConsolidateDecayPreview(ctx, store, agentName, memoryConsolidateHalfLife, memoryConsolidateDecayLimit); err != nil {
			return err
		}
	}

	if apply {
		if memoryConsolidateSkipDecay {
			fmt.Printf("consolidate: apply complete (promoted=%d). Decay preview skipped; no fragments were deleted.\n", promoted)
		} else {
			fmt.Printf("consolidate: apply complete (promoted=%d). Decay preview was non-destructive; no fragments were deleted.\n", promoted)
		}
	} else {
		fmt.Println("consolidate: dry-run complete. No files, fragments, or decay scores were modified.")
	}
	return nil
}

func memoryConsolidateApplyMode(globalDryRun, applyFlag bool) (bool, error) {
	if globalDryRun && applyFlag {
		return false, fmt.Errorf("--dry-run and --apply cannot be used together")
	}
	return applyFlag && !globalDryRun, nil
}

func printMemoryConsolidateDecayPreview(ctx context.Context, store *memorydb.Store, agent string, halfLifeDays float64, limit int) error {
	staleCount, err := store.CountDecayCandidates(ctx, agent, halfLifeDays, memorydb.DefaultDecayGCThreshold)
	if err != nil {
		return err
	}

	fmt.Printf("decay preview: %d fragment(s) would be below %.2f with half-life %.1f days; no scores changed and no fragments deleted.\n",
		staleCount, memorydb.DefaultDecayGCThreshold, halfLifeDays)
	if limit == 0 {
		return nil
	}

	previews, err := store.PreviewDecayScores(ctx, agent, halfLifeDays, limit)
	if err != nil {
		return err
	}
	if len(previews) == 0 {
		return nil
	}

	fmt.Println("lowest preview scores:")
	for _, preview := range previews {
		fmt.Printf("  %.3f current=%.3f %s\n", preview.PreviewScore, preview.CurrentScore, preview.Path)
	}
	return nil
}
