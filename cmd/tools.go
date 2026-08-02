package cmd

import (
	"context"
	"fmt"
	"log"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/search"
	"github.com/samsaffron/term-llm/internal/tools"
)

type unavailableGatewaySearcher struct{ err error }

func (s unavailableGatewaySearcher) Search(context.Context, string, int) ([]search.Result, error) {
	return nil, fmt.Errorf("gateway search unavailable: %w; check gateway URL/network/token or set gateway.search: false to use local search", s.err)
}

type unavailableGatewayFetcher struct{ err error }

func (f unavailableGatewayFetcher) FetchURL(context.Context, string) (string, error) {
	return "", fmt.Errorf("gateway read_url unavailable: %w; check gateway URL/network/token or set gateway.fetch: false to use local fetch", f.err)
}

func defaultToolRegistry(cfg *config.Config) *llm.ToolRegistry {
	registry := llm.NewToolRegistry()
	searcher, err := search.NewSearcher(cfg)
	if err != nil {
		if cfg != nil && cfg.Gateway.Enabled() && cfg.Gateway.RouteSearch() {
			log.Printf("Warning: gateway search unavailable: %v", err)
			searcher = unavailableGatewaySearcher{err: err}
		} else {
			log.Printf("Warning: search provider error: %v, falling back to DuckDuckGo", err)
			searcher = search.NewDuckDuckGoLite(nil)
		}
	}
	registry.Register(llm.NewWebSearchTool(searcher))
	if readURLTool := newReadURLToolForConfig(cfg); readURLTool != nil {
		registry.Register(readURLTool)
	}
	return registry
}

func newReadURLToolForConfig(cfg *config.Config) *llm.ReadURLTool {
	if cfg.Gateway.Enabled() && cfg.Gateway.RouteFetch() {
		client, err := search.NewGatewayClient(cfg.Gateway)
		if err != nil {
			log.Printf("Warning: gateway fetch unavailable: %v", err)
			return llm.NewReadURLToolWithFetcher(unavailableGatewayFetcher{err: err})
		}
		return llm.NewReadURLToolWithFetcher(client)
	}
	switch cfg.Search.FetchProvider {
	case "", "jina":
		return llm.NewReadURLTool()
	case "exa_mcp":
		return llm.NewReadURLToolWithFetcher(search.NewExaMCPClient(cfg.Search.ExaMCP.URL, cfg.Search.ExaMCP.APIKey))
	case "none":
		return nil
	default:
		log.Printf("Warning: unknown fetch provider %q, falling back to Jina", cfg.Search.FetchProvider)
		return llm.NewReadURLTool()
	}
}

// newEngine creates an Engine with the default tool registry and global config
// applied (e.g., tool output truncation). All command entry points should use
// this instead of calling llm.NewEngine directly.
func newEngine(provider llm.Provider, cfg *config.Config) *llm.Engine {
	engine := llm.NewEngine(provider, defaultToolRegistry(cfg))
	engine.SetMaxToolOutputChars(cfg.Tools.MaxToolOutputChars)
	return engine
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

func applySpawnConfig(toolConfig *tools.ToolConfig, spawn tools.SpawnConfig) {
	if spawn.MaxParallel > 0 {
		toolConfig.Spawn.MaxParallel = spawn.MaxParallel
	}
	if spawn.MaxDepth > 0 {
		toolConfig.Spawn.MaxDepth = spawn.MaxDepth
	}
	if spawn.DefaultTimeout > 0 {
		toolConfig.Spawn.DefaultTimeout = spawn.DefaultTimeout
	}
	if len(spawn.AllowedAgents) > 0 {
		toolConfig.Spawn.AllowedAgents = append([]string(nil), spawn.AllowedAgents...)
	}
	if len(spawn.AgentModels) > 0 {
		toolConfig.Spawn.AgentModels = cloneStringMap(spawn.AgentModels)
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]string, len(values))
	for k, v := range values {
		clone[k] = v
	}
	return clone
}

// filterOutTools removes specified tools from the enabled list.
func filterOutTools(enabled []string, exclude ...string) []string {
	excludeSet := make(map[string]bool)
	for _, e := range exclude {
		excludeSet[e] = true
	}
	var filtered []string
	for _, t := range enabled {
		if !excludeSet[t] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}
