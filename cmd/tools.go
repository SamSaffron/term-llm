package cmd

import (
	"log"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/search"
	"github.com/samsaffron/term-llm/internal/tools"
)

func defaultToolRegistry(cfg *config.Config) *llm.ToolRegistry {
	registry := llm.NewToolRegistry()
	searcher, err := search.NewSearcher(cfg)
	if err != nil {
		log.Printf("Warning: search provider error: %v, falling back to DuckDuckGo", err)
		searcher = search.NewDuckDuckGoLite(nil)
	}
	registry.Register(llm.NewWebSearchTool(searcher))
	registry.Register(llm.NewReadURLTool())
	return registry
}

// buildToolConfig creates a ToolConfig from CLI flags and config defaults.
func buildToolConfig(toolsFlag string, readDirs, writeDirs, shellAllow []string, cfg *config.Config) tools.ToolConfig {
	// Start with config defaults
	tc := tools.ToolConfig{
		Enabled:         cfg.Tools.Enabled,
		ReadDirs:        cfg.Tools.ReadDirs,
		WriteDirs:       cfg.Tools.WriteDirs,
		ShellAllow:      cfg.Tools.ShellAllow,
		ShellAutoRun:    cfg.Tools.ShellAutoRun,
		ShellAutoRunEnv: cfg.Tools.ShellAutoRunEnv,
		ShellNonTTYEnv:  cfg.Tools.ShellNonTTYEnv,
		ImageProvider:   cfg.Tools.ImageProvider,
	}

	// Override with CLI flags
	if toolsFlag != "" {
		tc.Enabled = tools.ParseToolsFlag(toolsFlag)
	}
	if len(readDirs) > 0 {
		tc.ReadDirs = append(tc.ReadDirs, readDirs...)
	}
	if len(writeDirs) > 0 {
		tc.WriteDirs = append(tc.WriteDirs, writeDirs...)
	}
	if len(shellAllow) > 0 {
		tc.ShellAllow = append(tc.ShellAllow, shellAllow...)
	}

	return tc
}
