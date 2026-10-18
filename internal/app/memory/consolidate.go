package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

// ConsolidateOptions configures `memory consolidate`.
type ConsolidateOptions struct {
	Env
	Apply          bool
	Since          time.Duration
	RecentMaxBytes int
	Model          string
	HalfLife       float64
	DecayLimit     int
	SkipDecay      bool
}

// Consolidate promotes changed fragments into recent.md and previews decay.
func Consolidate(opts ConsolidateOptions) error {
	env := opts.Env
	if opts.RecentMaxBytes <= 0 {
		return fmt.Errorf("--recent-max-bytes must be > 0")
	}
	if opts.HalfLife <= 0 {
		return fmt.Errorf("--half-life must be > 0")
	}
	if opts.DecayLimit < 0 {
		return fmt.Errorf("--decay-limit must be >= 0")
	}

	apply, err := memoryConsolidateApplyMode(env.DryRun, opts.Apply)
	if err != nil {
		return err
	}

	cfg, err := env.LoadConfig()
	if err != nil {
		return err
	}
	if err := env.ApplyAskProviderOverrides(cfg); err != nil {
		return err
	}
	if strings.TrimSpace(opts.Model) != "" {
		cfg.ApplyOverrides("", strings.TrimSpace(opts.Model))
	}

	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return err
	}
	engine := env.NewEngine(provider, cfg)

	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	agentName := env.AgentFilter()
	if agentName == "" {
		agentName = env.ResolveAgent("")
	}

	if !apply {
		fmt.Println("consolidate: dry-run (no writes). Proposed recent.md follows; rerun with --apply to write it.")
	}

	promoted, err := runMemoryPromoteFlow(ctx, env, cfg, engine, store, memoryPromoteOptions{
		Agent:          agentName,
		Since:          opts.Since,
		RecentMaxBytes: opts.RecentMaxBytes,
		Model:          strings.TrimSpace(opts.Model),
		DryRun:         !apply,
	})
	if err != nil {
		return err
	}

	if !opts.SkipDecay {
		if err := printMemoryConsolidateDecayPreview(ctx, store, agentName, opts.HalfLife, opts.DecayLimit); err != nil {
			return err
		}
	}

	if apply {
		if opts.SkipDecay {
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
