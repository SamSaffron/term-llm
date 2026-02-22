package cmd

import (
	"fmt"
	"os"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
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

	memoryCmd.AddCommand(memoryMineCmd)
	memoryCmd.AddCommand(memorySearchCmd)
	memoryCmd.AddCommand(memoryStatusCmd)
	memoryCmd.AddCommand(memoryFragmentsCmd)
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
