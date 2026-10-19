package cmd

import (
	"github.com/samsaffron/term-llm/internal/app/memory"
	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/image"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
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
	defaultDB := memory.DefaultDBPath()
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

// memoryEnv builds the shared memory command environment from the persistent
// flags and CLI helpers.
func memoryEnv() memory.Env {
	return memory.Env{
		Agent:         memoryAgent,
		DBPath:        memoryDBPath,
		DefaultDBPath: defaultMemoryDBPath,
		DryRun:        memoryDryRun,
		Deps: memory.Deps{
			LoadConfig: loadConfig,
			ApplyAskProviderOverrides: func(cfg *config.Config) error {
				return applyProviderOverridesWithAgent(cfg, cfg.Ask.Provider, cfg.Ask.Model, "", "", "")
			},
			NewEngine: newEngine,
			SessionStoreConfig: func(cfg *config.Config) session.Config {
				return appruntime.SessionStoreConfigWith(cfg, appruntime.SessionStoreOverrides{NoSession: noSession, DBPath: sessionDBPath})
			},
			ParseTags:          parseTags,
			FormatRelativeTime: formatRelativeTime,
			FormatBytes:        formatBytes,
			ActiveModel:        activeModel,
			LoadAgent:          LoadAgent,
			DebugRaw:           func() bool { return debugRaw },
		},
	}
}

func openMemoryStore() (*memorydb.Store, error) {
	return memoryEnv().OpenStore()
}

// wireImageRecorder attaches a lazy image recorder to the tool registry.
func wireImageRecorder(registry *tools.LocalToolRegistry, agent, sessionID string) {
	memory.WireImageRecorder(registry, memoryDBPath, defaultMemoryDBPath, agent, sessionID)
}

func recordImageDirect(cfg *config.Config, prompt, outputPath string, result *image.ImageResult, providerName string) {
	memory.RecordImageDirect(memoryDBPath, defaultMemoryDBPath, cfg, prompt, outputPath, result, providerName)
}
