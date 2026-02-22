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
	memoryCmd.AddCommand(memorySearchCmd)
	memoryCmd.AddCommand(memoryPromoteCmd)
	memoryCmd.AddCommand(memoryStatusCmd)
	memoryCmd.AddCommand(memoryFragmentsCmd)
	memoryCmd.AddCommand(memoryImagesCmd)
}

func openMemoryStore() (*memorydb.Store, error) {
	if dbEnv := os.Getenv("TERM_LLM_MEMORY_DB"); dbEnv != "" {
		if memoryDBPath == defaultMemoryDBPath {
			memoryDBPath = dbEnv
		}
	}
	store, err := memorydb.NewStore(memorydb.Config{Path: memoryDBPath})
	if err != nil {
		return nil, fmt.Errorf("open memory store: %w", err)
	}
	return store, nil
}

// wireImageRecorder opens the memory store and attaches it to the registry as an image recorder.
// Non-fatal: if the store cannot be opened, image generation still works normally.
func wireImageRecorder(registry *tools.LocalToolRegistry, agent, sessionID string) {
	if registry == nil {
		return
	}
	store, err := openMemoryStore()
	if err != nil {
		return
	}
	registry.SetImageRecorder(store, agent, sessionID)
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
