package memory

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

// InsightsListOptions configures `memory insights list`.
type InsightsListOptions struct {
	Env
	Limit int
}

// InsightsAddOptions configures `memory insights add`.
type InsightsAddOptions struct {
	Env
	Content    string
	Compact    string
	Category   string
	Trigger    string
	Confidence float64
}

// InsightsUpdateOptions configures `memory insights update`.
type InsightsUpdateOptions struct {
	Env
	Content string
	Compact string
}

// InsightsSearchOptions configures `memory insights search`.
type InsightsSearchOptions struct {
	Env
	Limit int
}

// InsightsExpandOptions configures `memory insights expand`.
type InsightsExpandOptions struct {
	Env
	MaxTokens int
}

// InsightsDecayOptions configures `memory insights decay`.
type InsightsDecayOptions struct {
	Env
	HalfLifeDays float64
}

// InsightsGCOptions configures `memory insights gc`.
type InsightsGCOptions struct {
	Env
	MinConfidence float64
}

// InsightsDeleteOptions configures `memory insights delete`.
type InsightsDeleteOptions struct {
	Env
}

// InsightsReinforceOptions configures `memory insights reinforce`.
type InsightsReinforceOptions struct {
	Env
}

// InsightsList runs `memory insights list`.
func InsightsList(opts InsightsListOptions) error {
	env := opts.Env
	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	agent := env.AgentFilter()
	insights, err := store.ListInsights(context.Background(), agent, opts.Limit)
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

// InsightsAdd runs `memory insights add`.
func InsightsAdd(opts InsightsAddOptions) error {
	env := opts.Env
	agent := env.AgentFilter()
	if agent == "" {
		return fmt.Errorf("--agent is required")
	}
	content, err := readFragmentContent(opts.Content)
	if err != nil {
		return err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("--content is required (or pipe content via stdin)")
	}

	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ins := &memorydb.Insight{
		Agent:          agent,
		Content:        content,
		CompactContent: strings.TrimSpace(opts.Compact),
		Category:       strings.TrimSpace(opts.Category),
		TriggerDesc:    strings.TrimSpace(opts.Trigger),
		Confidence:     opts.Confidence,
	}
	if err := store.CreateInsight(context.Background(), ins); err != nil {
		return err
	}
	fmt.Printf("created insight: id=%d\n", ins.ID)
	return nil
}

// InsightsUpdate runs `memory insights update`.
func InsightsUpdate(opts InsightsUpdateOptions, args []string) error {
	env := opts.Env
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("id must be an integer: %w", err)
	}
	content, err := readFragmentContent(opts.Content)
	if err != nil {
		return err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("--content is required (or pipe content via stdin)")
	}

	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.UpdateInsight(context.Background(), id, content, strings.TrimSpace(opts.Compact)); err != nil {
		return err
	}
	fmt.Printf("updated insight: id=%d\n", id)
	return nil
}

// InsightsDelete runs `memory insights delete`.
func InsightsDelete(opts InsightsDeleteOptions, args []string) error {
	env := opts.Env
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("id must be an integer: %w", err)
	}

	store, err := env.OpenStore()
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

// InsightsReinforce runs `memory insights reinforce`.
func InsightsReinforce(opts InsightsReinforceOptions, args []string) error {
	env := opts.Env
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("id must be an integer: %w", err)
	}

	store, err := env.OpenStore()
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

// InsightsSearch runs `memory insights search`.
func InsightsSearch(opts InsightsSearchOptions, args []string) error {
	env := opts.Env
	query := strings.TrimSpace(args[0])
	agent := env.AgentFilter()

	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	insights, err := store.SearchInsights(context.Background(), agent, query, opts.Limit)
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

// InsightsExpand runs `memory insights expand`.
func InsightsExpand(opts InsightsExpandOptions, args []string) error {
	env := opts.Env
	query := strings.TrimSpace(args[0])
	agent := env.AgentFilter()

	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	maxTok := opts.MaxTokens
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

type insightMaintenanceKind int

const (
	insightMaintenanceDecay insightMaintenanceKind = iota
	insightMaintenanceGC
)

type insightMaintenanceSpec struct {
	preview       func(*memorydb.Store, context.Context, string, float64) (int, error)
	apply         func(*memorydb.Store, context.Context, string, float64) (int, error)
	previewFormat string
	applyFormat   string
}

var insightMaintenanceSpecs = map[insightMaintenanceKind]insightMaintenanceSpec{
	insightMaintenanceDecay: {
		preview: func(store *memorydb.Store, ctx context.Context, agent string, value float64) (int, error) {
			return countInsightDecayPreview(ctx, store, agent, value)
		},
		apply:         (*memorydb.Store).DecayInsights,
		previewFormat: "decay: would update %d insights (half-life=%.1f days; dry-run, no changes)\n",
		applyFormat:   "decayed %d insights (half-life=%.1f days)\n",
	},
	insightMaintenanceGC: {
		preview: func(store *memorydb.Store, ctx context.Context, agent string, value float64) (int, error) {
			return countInsightGCCandidates(ctx, store, agent, value)
		},
		apply:         (*memorydb.Store).GCInsights,
		previewFormat: "gc: would delete %d insights below confidence %.2f (dry-run, no changes)\n",
		applyFormat:   "gc: deleted %d insights below confidence %.2f\n",
	},
}

// InsightsDecay runs `memory insights decay`.
func InsightsDecay(opts InsightsDecayOptions) error {
	env := opts.Env
	return runInsightMaintenance(env, insightMaintenanceDecay, opts.HalfLifeDays)
}

// InsightsGC runs `memory insights gc`.
func InsightsGC(opts InsightsGCOptions) error {
	env := opts.Env
	return runInsightMaintenance(env, insightMaintenanceGC, opts.MinConfidence)
}

func runInsightMaintenance(env Env, kind insightMaintenanceKind, value float64) error {
	spec, ok := insightMaintenanceSpecs[kind]
	if !ok {
		return fmt.Errorf("unknown insight maintenance operation %d", kind)
	}
	store, err := env.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()
	operation := spec.apply
	format := spec.applyFormat
	if env.DryRun {
		operation = spec.preview
		format = spec.previewFormat
	}
	n, err := operation(store, context.Background(), env.AgentFilter(), value)
	if err != nil {
		return err
	}
	fmt.Printf(format, n, value)
	return nil
}

func countInsightDecayPreview(ctx context.Context, store *memorydb.Store, agent string, halfLifeDays float64) (int, error) {
	if halfLifeDays <= 0 {
		halfLifeDays = memorydb.DefaultDecayHalfLifeDays
	}
	insights, err := store.ListInsights(ctx, agent, 0)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	count := 0
	for _, ins := range insights {
		if ins == nil || ins.LastReinforced.IsZero() {
			continue
		}
		daysSince := now.Sub(ins.LastReinforced).Hours() / 24
		if daysSince < 1 {
			continue
		}
		decayed := ins.Confidence * math.Pow(2, -daysSince/halfLifeDays)
		if math.Abs(decayed-ins.Confidence) >= 0.001 {
			count++
		}
	}
	return count, nil
}

func countInsightGCCandidates(ctx context.Context, store *memorydb.Store, agent string, minConfidence float64) (int, error) {
	if minConfidence <= 0 {
		minConfidence = 0.1
	}
	insights, err := store.ListInsights(ctx, agent, 0)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, ins := range insights {
		if ins != nil && ins.Confidence < minConfidence {
			count++
		}
	}
	return count, nil
}
