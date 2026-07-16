package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/image"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
)

var (
	memoryAgent         string
	memoryDBPath        string
	memoryDryRun        bool
	defaultMemoryDBPath string
)

var memoryCmd = &cobra.Command{
	Use:   "memory",
	Short: "Mine and query long-term memory fragments",
	Long: `Manage long-term memory fragments mined from completed sessions.

Examples:
  term-llm memory mine
  term-llm memory search "retry policy"
  term-llm memory consolidate --dry-run
  term-llm memory status
  term-llm memory fragments list`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

func init() {
	defaultDB := "~/.local/share/term-llm/memory.db"
	if dbPath, err := memorydb.GetDBPath(); err == nil {
		defaultDB = dbPath
	}
	defaultMemoryDBPath = defaultDB

	memoryCmd.PersistentFlags().StringVar(&memoryAgent, "agent", "", "Filter by agent")
	memoryCmd.PersistentFlags().StringVar(&memoryDBPath, "db", defaultDB, "Override memory database path")
	memoryCmd.PersistentFlags().BoolVar(&memoryDryRun, "dry-run", false, "Preview actions without writing changes")
	memoryCmd.RegisterFlagCompletionFunc("agent", memoryAgentCompletion)

	memoryCmd.AddCommand(memoryMineCmd)
	memoryCmd.AddCommand(memoryUpdateRecentCmd)
	memoryCmd.AddCommand(memorySearchCmd)
	memoryCmd.AddCommand(memoryPromoteCmd)
	memoryCmd.AddCommand(memoryConsolidateCmd)
	memoryCmd.AddCommand(memoryStatusCmd)
	memoryCmd.AddCommand(memoryFragmentsCmd)
	memoryCmd.AddCommand(memoryImagesCmd)
	memoryCmd.AddCommand(memoryInsightsCmd)
}

type imageRecordStore interface {
	tools.ImageRecorder
	Close() error
}

type imageRecordStoreOpener func(path string) (imageRecordStore, error)

type lazyImageRecorder struct {
	path string
	open imageRecordStoreOpener
}

func (r *lazyImageRecorder) RecordImage(ctx context.Context, record *memorydb.ImageRecord) error {
	store, err := r.open(r.path)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.RecordImage(ctx, record)
}

func resolvedMemoryDBPath() string {
	if dbEnv := os.Getenv("TERM_LLM_MEMORY_DB"); dbEnv != "" && memoryDBPath == defaultMemoryDBPath {
		return dbEnv
	}
	return memoryDBPath
}

func openMemoryStoreAtPath(path string) (*memorydb.Store, error) {
	store, err := memorydb.NewStore(memorydb.Config{Path: path})
	if err != nil {
		return nil, fmt.Errorf("open memory store: %w", err)
	}
	return store, nil
}

func openMemoryStore() (*memorydb.Store, error) {
	return openMemoryStoreAtPath(resolvedMemoryDBPath())
}

func openImageRecordStore(path string) (imageRecordStore, error) {
	return openMemoryStoreAtPath(path)
}

// wireImageRecorder attaches a recorder that opens the memory store only when
// an image is generated, then closes it after recording. Recording remains
// non-fatal if the store cannot be opened.
func wireImageRecorder(registry *tools.LocalToolRegistry, agent, sessionID string) {
	wireImageRecorderWithOpener(registry, agent, sessionID, openImageRecordStore)
}

func wireImageRecorderWithOpener(registry *tools.LocalToolRegistry, agent, sessionID string, open imageRecordStoreOpener) {
	if registry == nil {
		return
	}
	recorder := &lazyImageRecorder{
		// Runtime setup has already applied flags and environment overrides. Keep
		// that selected path stable for the lifetime of this registry.
		path: resolvedMemoryDBPath(),
		open: open,
	}
	registry.SetImageRecorder(recorder, agent, sessionID)
}

func recordImageDirect(cfg *config.Config, prompt, outputPath string, result *image.ImageResult, providerName string) {
	if cfg == nil || outputPath == "" || result == nil {
		return
	}
	store, err := openMemoryStore()
	if err != nil {
		return
	}
	defer store.Close()

	rec := &memorydb.ImageRecord{
		Prompt:     prompt,
		OutputPath: outputPath,
		MimeType:   result.MimeType,
		Provider:   providerName,
		FileSize:   len(result.Data),
	}
	_ = store.RecordImage(context.Background(), rec)
}
