package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/spf13/cobra"
)

var (
	memoryFragmentsSince    time.Duration
	memoryFragmentsLimit    int
	memoryFragmentsHalfLife float64
)

var memoryFragmentsCmd = &cobra.Command{
	Use:   "fragments",
	Short: "Inspect stored memory fragments",
}

var memoryFragmentsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List memory fragments",
	RunE:  runMemoryFragmentsList,
}

var memoryFragmentsShowCmd = &cobra.Command{
	Use:   "show <path>",
	Short: "Show a memory fragment by path",
	Args:  cobra.ExactArgs(1),
	RunE:  runMemoryFragmentsShow,
}

var memoryFragmentsGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage collect decayed memory fragments",
	RunE:  runMemoryFragmentsGC,
}

func init() {
	memoryFragmentsCmd.AddCommand(memoryFragmentsListCmd)
	memoryFragmentsCmd.AddCommand(memoryFragmentsShowCmd)
	memoryFragmentsCmd.AddCommand(memoryFragmentsGCCmd)

	memoryFragmentsListCmd.Flags().DurationVar(&memoryFragmentsSince, "since", 0, "Only show fragments updated within this duration (e.g. 24h)")
	memoryFragmentsListCmd.Flags().IntVar(&memoryFragmentsLimit, "limit", 0, "Maximum number of fragments to return (0 = all)")
	memoryFragmentsGCCmd.Flags().Float64Var(&memoryFragmentsHalfLife, "half-life", 30.0, "Decay half-life in days for GC recalculation")
	// --dry-run is inherited from the root memory command's persistent flags.
}

func runMemoryFragmentsList(cmd *cobra.Command, args []string) error {
	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	opts := memorydb.ListOptions{
		Agent: strings.TrimSpace(memoryAgent),
		Limit: memoryFragmentsLimit,
	}
	if memoryFragmentsSince > 0 {
		cutoff := time.Now().Add(-memoryFragmentsSince)
		opts.Since = &cutoff
	}

	fragments, err := store.ListFragments(context.Background(), opts)
	if err != nil {
		return err
	}
	if len(fragments) == 0 {
		fmt.Println("No memory fragments found.")
		return nil
	}

	if strings.TrimSpace(memoryAgent) == "" {
		fmt.Printf("%-14s %-42s %-10s\n", "AGENT", "PATH", "UPDATED")
		fmt.Println(strings.Repeat("-", 72))
		for _, f := range fragments {
			fmt.Printf("%-14s %-42s %-10s\n", f.Agent, truncateString(f.Path, 42), formatRelativeTime(f.UpdatedAt))
		}
		return nil
	}

	fmt.Printf("%-42s %-10s\n", "PATH", "UPDATED")
	fmt.Println(strings.Repeat("-", 56))
	for _, f := range fragments {
		fmt.Printf("%-42s %-10s\n", truncateString(f.Path, 42), formatRelativeTime(f.UpdatedAt))
	}

	return nil
}

func runMemoryFragmentsShow(cmd *cobra.Command, args []string) error {
	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	fragPath := strings.TrimSpace(args[0])
	if fragPath == "" {
		return fmt.Errorf("path cannot be empty")
	}

	if strings.TrimSpace(memoryAgent) != "" {
		frag, err := store.GetFragment(ctx, strings.TrimSpace(memoryAgent), fragPath)
		if err != nil {
			return err
		}
		if frag == nil {
			return fmt.Errorf("fragment not found: %s", fragPath)
		}
		fmt.Print(frag.Content)
		if !strings.HasSuffix(frag.Content, "\n") {
			fmt.Println()
		}
		return nil
	}

	frags, err := store.FindFragmentsByPath(ctx, fragPath)
	if err != nil {
		return err
	}
	if len(frags) == 0 {
		return fmt.Errorf("fragment not found: %s", fragPath)
	}
	if len(frags) > 1 {
		return fmt.Errorf("fragment path %q exists for multiple agents; rerun with --agent", fragPath)
	}

	fmt.Print(frags[0].Content)
	if !strings.HasSuffix(frags[0].Content, "\n") {
		fmt.Println()
	}
	return nil
}

func runMemoryFragmentsGC(cmd *cobra.Command, args []string) error {
	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	agent := strings.TrimSpace(memoryAgent)

	if memoryDryRun {
		count, err := store.CountGCCandidates(ctx, agent)
		if err != nil {
			return err
		}
		fmt.Printf("gc: would remove %d fragments (based on current decay scores, no recalc in dry-run)\n", count)
		return nil
	}

	if _, err := store.RecalcDecayScores(ctx, agent, memoryFragmentsHalfLife); err != nil {
		return fmt.Errorf("recalculate decay scores: %w", err)
	}

	removed, err := store.GCFragments(ctx, agent)
	if err != nil {
		return err
	}
	fmt.Printf("gc: removed %d fragments\n", removed)
	return nil
}
