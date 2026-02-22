package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/spf13/cobra"
)

var (
	memoryFragmentsSince    time.Duration
	memoryFragmentsLimit    int
	memoryFragmentsHalfLife float64
	memoryFragmentsSyncDir  string
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

var memoryFragmentsSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync on-disk fragment .md files into the memory database",
	Long: `Walks a directory of .md files and upserts each into the memory database.

Fragments are stored under <dir>/<category>/<topic>/<fact>.md (any depth).
The path stored in the DB is relative to <dir>, prefixed with "fragments/".

Use --agent to set the owning agent (required).
Use --dir to specify the fragments root directory (required).

New files are created; files whose content has changed are updated (embeddings
are cleared and will be regenerated on the next 'memory mine --embed' run).
Files already in the DB with identical content are skipped.`,
	RunE: runMemoryFragmentsSync,
}

func init() {
	memoryFragmentsCmd.AddCommand(memoryFragmentsListCmd)
	memoryFragmentsCmd.AddCommand(memoryFragmentsShowCmd)
	memoryFragmentsCmd.AddCommand(memoryFragmentsGCCmd)
	memoryFragmentsCmd.AddCommand(memoryFragmentsSyncCmd)

	memoryFragmentsListCmd.Flags().DurationVar(&memoryFragmentsSince, "since", 0, "Only show fragments updated within this duration (e.g. 24h)")
	memoryFragmentsListCmd.Flags().IntVar(&memoryFragmentsLimit, "limit", 0, "Maximum number of fragments to return (0 = all)")
	memoryFragmentsGCCmd.Flags().Float64Var(&memoryFragmentsHalfLife, "half-life", 30.0, "Decay half-life in days for GC recalculation")
	memoryFragmentsSyncCmd.Flags().StringVar(&memoryFragmentsSyncDir, "dir", "", "Root directory containing .md fragment files (required)")
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

func runMemoryFragmentsSync(cmd *cobra.Command, args []string) error {
	agent := strings.TrimSpace(memoryAgent)
	if agent == "" {
		return fmt.Errorf("--agent is required")
	}
	dir := strings.TrimSpace(memoryFragmentsSyncDir)
	if dir == "" {
		return fmt.Errorf("--dir is required")
	}

	// Resolve to absolute path
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}
	if _, err := os.Stat(absDir); err != nil {
		return fmt.Errorf("dir not found: %w", err)
	}

	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	var created, updated, skipped, errors int

	err = filepath.Walk(absDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}

		// Compute DB path: "fragments/<relative-from-dir>"
		rel, err := filepath.Rel(absDir, path)
		if err != nil {
			return err
		}
		dbPath := "fragments/" + filepath.ToSlash(rel)

		content, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  error reading %s: %v\n", rel, err)
			errors++
			return nil
		}
		contentStr := string(content)
		if strings.TrimSpace(contentStr) == "" {
			skipped++
			return nil
		}

		existing, err := store.GetFragment(ctx, agent, dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  error checking %s: %v\n", dbPath, err)
			errors++
			return nil
		}

		if existing != nil {
			if existing.Content == contentStr {
				// Identical — nothing to do
				skipped++
				return nil
			}
			// Content changed — update
			if memoryDryRun {
				fmt.Printf("  would update: %s\n", dbPath)
				updated++
				return nil
			}
			_, err = store.UpdateFragment(ctx, agent, dbPath, contentStr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  error updating %s: %v\n", dbPath, err)
				errors++
				return nil
			}
			fmt.Printf("  updated: %s\n", dbPath)
			updated++
			return nil
		}

		// New fragment — create it, using file mtime for timestamps
		mtime := info.ModTime()
		frag := &memorydb.Fragment{
			Agent:     agent,
			Path:      dbPath,
			Content:   contentStr,
			Source:    "sync",
			CreatedAt: mtime,
			UpdatedAt: mtime,
		}
		if memoryDryRun {
			fmt.Printf("  would create: %s\n", dbPath)
			created++
			return nil
		}
		if err := store.CreateFragment(ctx, frag); err != nil {
			fmt.Fprintf(os.Stderr, "  error creating %s: %v\n", dbPath, err)
			errors++
			return nil
		}
		fmt.Printf("  created: %s\n", dbPath)
		created++
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk error: %w", err)
	}

	action := "sync"
	if memoryDryRun {
		action = "dry-run"
	}
	fmt.Printf("\n%s complete: %d created, %d updated, %d skipped, %d errors\n",
		action, created, updated, skipped, errors)
	return nil
}
