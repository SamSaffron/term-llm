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
	Use:               "show <path>",
	Short:             "Show a memory fragment by path",
	Args:              cobra.ExactArgs(1),
	RunE:              runMemoryFragmentsShow,
	ValidArgsFunction: memoryFragmentPathCompletion,
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

// shortFragmentID returns the last 6-char hex suffix of a fragment ID for display.
// e.g. "mem-20260223-142301-a3f9c1" -> "a3f9c1"
// Falls back to the full ID if the format is unexpected.
func shortFragmentID(id string) string {
	if idx := strings.LastIndex(id, "-"); idx >= 0 && len(id)-idx-1 == 6 {
		return id[idx+1:]
	}
	return id
}

// isHexString returns true if s consists entirely of lowercase hex characters.
func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
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
		fmt.Printf("%-8s %-14s %-10s %s\n", "ID", "AGENT", "UPDATED", "PATH")
		fmt.Println(strings.Repeat("-", 90))
		for _, f := range fragments {
			fmt.Printf("%-8s %-14s %-10s %s\n", shortFragmentID(f.ID), f.Agent, formatRelativeTime(f.UpdatedAt), f.Path)
		}
		return nil
	}

	fmt.Printf("%-8s %-10s %s\n", "ID", "UPDATED", "PATH")
	fmt.Println(strings.Repeat("-", 72))
	for _, f := range fragments {
		fmt.Printf("%-8s %-10s %s\n", shortFragmentID(f.ID), formatRelativeTime(f.UpdatedAt), f.Path)
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
	arg := strings.TrimSpace(args[0])
	if arg == "" {
		return fmt.Errorf("path or id cannot be empty")
	}

	// Full ID: mem-20260223-142301-a3f9c1
	if strings.HasPrefix(arg, "mem-") {
		frag, err := store.GetFragmentByID(ctx, arg)
		if err != nil {
			return err
		}
		if frag == nil {
			return fmt.Errorf("fragment not found: %s", arg)
		}
		fmt.Print(frag.Content)
		if !strings.HasSuffix(frag.Content, "\n") {
			fmt.Println()
		}
		return nil
	}

	// Short ID: 6-char hex suffix (e.g. "a3f9c1")
	if len(arg) == 6 && isHexString(arg) {
		frag, err := store.GetFragmentByShortID(ctx, arg)
		if err != nil {
			return err
		}
		if frag == nil {
			return fmt.Errorf("fragment not found: %s", arg)
		}
		fmt.Print(frag.Content)
		if !strings.HasSuffix(frag.Content, "\n") {
			fmt.Println()
		}
		return nil
	}

	// Otherwise resolve by path (existing behaviour)
	if strings.TrimSpace(memoryAgent) != "" {
		frag, err := store.GetFragment(ctx, strings.TrimSpace(memoryAgent), arg)
		if err != nil {
			return err
		}
		if frag == nil {
			return fmt.Errorf("fragment not found: %s", arg)
		}
		fmt.Print(frag.Content)
		if !strings.HasSuffix(frag.Content, "\n") {
			fmt.Println()
		}
		return nil
	}

	frags, err := store.FindFragmentsByPath(ctx, arg)
	if err != nil {
		return err
	}
	if len(frags) == 0 {
		return fmt.Errorf("fragment not found: %s", arg)
	}
	if len(frags) > 1 {
		return fmt.Errorf("fragment path %q exists for multiple agents; rerun with --agent or use the ID", arg)
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
