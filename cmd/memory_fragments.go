package cmd

import (
	"time"

	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/spf13/cobra"
)

var (
	memoryFragmentsSince        time.Duration
	memoryFragmentsLimit        int
	memoryFragmentsHalfLife     float64
	memoryFragmentsPreviewDecay bool
	memoryFragmentsDelete       bool
	memoryFragmentsSyncDir      string
	memoryFragmentsShowJSON     bool
	memoryFragmentsShowNoPath   bool
	memoryFragmentsSourcesJSON  bool
	memoryFragmentsFilterPath   string

	memoryFragmentsAddContent    string
	memoryFragmentsAddSource     string
	memoryFragmentsUpdateContent string
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

var memoryFragmentsSourcesCmd = &cobra.Command{
	Use:               "sources <path-or-rowid>",
	Short:             "Show session turn-range sources for a memory fragment",
	Args:              cobra.ExactArgs(1),
	RunE:              runMemoryFragmentsSources,
	ValidArgsFunction: memoryFragmentPathCompletion,
}

var memoryFragmentsGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage collect fragments already below the decay threshold",
	Long: `Review or explicitly delete non-pinned fragments whose stored decay_score is
below the GC threshold. By default this command is non-destructive and reports
what would be removed. Pass --delete to actually delete stored-score candidates.
This command no longer recalculates decay scores before deleting; use
--preview-decay for a non-destructive age-based decay preview.`,
	RunE: runMemoryFragmentsGC,
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

var memoryFragmentsAddCmd = &cobra.Command{
	Use:   "add <path>",
	Short: "Add a memory fragment",
	Args:  cobra.ExactArgs(1),
	RunE:  runMemoryFragmentsAdd,
}

var memoryFragmentsUpdateCmd = &cobra.Command{
	Use:   "update <path>",
	Short: "Update a memory fragment",
	Args:  cobra.ExactArgs(1),
	RunE:  runMemoryFragmentsUpdate,
}

var memoryFragmentsDeleteCmd = &cobra.Command{
	Use:   "delete <path>",
	Short: "Delete a memory fragment",
	Args:  cobra.ExactArgs(1),
	RunE:  runMemoryFragmentsDelete,
}

func init() {
	memoryFragmentsCmd.AddCommand(memoryFragmentsListCmd)
	memoryFragmentsCmd.AddCommand(memoryFragmentsShowCmd)
	memoryFragmentsCmd.AddCommand(memoryFragmentsSourcesCmd)
	memoryFragmentsCmd.AddCommand(memoryFragmentsGCCmd)
	memoryFragmentsCmd.AddCommand(memoryFragmentsSyncCmd)
	memoryFragmentsCmd.AddCommand(memoryFragmentsAddCmd)
	memoryFragmentsCmd.AddCommand(memoryFragmentsUpdateCmd)
	memoryFragmentsCmd.AddCommand(memoryFragmentsDeleteCmd)

	memoryFragmentsListCmd.Flags().DurationVar(&memoryFragmentsSince, "since", 0, "Only show fragments updated within this duration (e.g. 24h)")
	memoryFragmentsListCmd.Flags().IntVar(&memoryFragmentsLimit, "limit", 0, "Maximum number of fragments to return (0 = all)")
	memoryFragmentsListCmd.Flags().StringVar(&memoryFragmentsFilterPath, "filter-path", "", "Filter fragments whose path contains this substring")
	memoryFragmentsGCCmd.Flags().Float64Var(&memoryFragmentsHalfLife, "half-life", memorydb.DefaultDecayHalfLifeDays, "Decay half-life in days for --preview-decay")
	memoryFragmentsGCCmd.Flags().BoolVar(&memoryFragmentsPreviewDecay, "preview-decay", false, "Compute non-destructive age-based decay candidates")
	memoryFragmentsGCCmd.Flags().BoolVar(&memoryFragmentsDelete, "delete", false, "Actually delete fragments already below the stored decay threshold")
	memoryFragmentsSyncCmd.Flags().StringVar(&memoryFragmentsSyncDir, "dir", "", "Root directory containing .md fragment files (required)")
	memoryFragmentsShowCmd.Flags().BoolVar(&memoryFragmentsShowJSON, "json", false, "Output fragment as JSON with all metadata")
	memoryFragmentsShowCmd.Flags().BoolVar(&memoryFragmentsShowNoPath, "no-path", false, "Suppress path header, print content only")
	memoryFragmentsSourcesCmd.Flags().BoolVar(&memoryFragmentsSourcesJSON, "json", false, "Output fragment sources as JSON")
	memoryFragmentsAddCmd.Flags().StringVar(&memoryFragmentsAddContent, "content", "", "Fragment content (defaults to stdin)")
	memoryFragmentsAddCmd.Flags().StringVar(&memoryFragmentsAddSource, "source", "manual", "Fragment source")
	memoryFragmentsUpdateCmd.Flags().StringVar(&memoryFragmentsUpdateContent, "content", "", "Fragment content (defaults to stdin)")
	// --dry-run is inherited from the root memory command's persistent flags.
}

func runMemoryFragmentsList(cmd *cobra.Command, args []string) error {
	return appmemory.FragmentsList(appmemory.FragmentsListOptions{
		Env:        memoryEnv(),
		Since:      memoryFragmentsSince,
		Limit:      memoryFragmentsLimit,
		FilterPath: memoryFragmentsFilterPath,
	})
}

func runMemoryFragmentsShow(cmd *cobra.Command, args []string) error {
	return appmemory.FragmentsShow(appmemory.FragmentsShowOptions{
		Env:    memoryEnv(),
		JSON:   memoryFragmentsShowJSON,
		NoPath: memoryFragmentsShowNoPath,
	}, args)
}

func runMemoryFragmentsSources(cmd *cobra.Command, args []string) error {
	return appmemory.FragmentsSources(appmemory.FragmentsSourcesOptions{
		Env:  memoryEnv(),
		JSON: memoryFragmentsSourcesJSON,
	}, args)
}

func runMemoryFragmentsGC(cmd *cobra.Command, args []string) error {
	return appmemory.FragmentsGC(appmemory.FragmentsGCOptions{
		Env:          memoryEnv(),
		HalfLife:     memoryFragmentsHalfLife,
		PreviewDecay: memoryFragmentsPreviewDecay,
		Delete:       memoryFragmentsDelete,
	})
}

func runMemoryFragmentsSync(cmd *cobra.Command, args []string) error {
	return appmemory.FragmentsSync(appmemory.FragmentsSyncOptions{
		Env: memoryEnv(),
		Dir: memoryFragmentsSyncDir,
	})
}

func runMemoryFragmentsAdd(cmd *cobra.Command, args []string) error {
	return appmemory.FragmentsAdd(appmemory.FragmentsAddOptions{
		Env:     memoryEnv(),
		Content: memoryFragmentsAddContent,
		Source:  memoryFragmentsAddSource,
	}, args)
}

func runMemoryFragmentsUpdate(cmd *cobra.Command, args []string) error {
	return appmemory.FragmentsUpdate(appmemory.FragmentsUpdateOptions{
		Env:     memoryEnv(),
		Content: memoryFragmentsUpdateContent,
	}, args)
}

func runMemoryFragmentsDelete(cmd *cobra.Command, args []string) error {
	return appmemory.FragmentsDelete(appmemory.FragmentsDeleteOptions{Env: memoryEnv()}, args)
}
