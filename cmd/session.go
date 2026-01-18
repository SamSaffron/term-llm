package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

// SessionSettings holds the resolved settings for a session, merged from
// config defaults, agent settings, and CLI flags.
type SessionSettings struct {
	// Provider/model
	Provider string
	Model    string

	// Tool settings
	Tools        string
	ReadDirs     []string
	WriteDirs    []string
	ShellAllow   []string
	ShellAutoRun bool
	Scripts      []string

	// MCP servers (comma-separated)
	MCP string

	// System prompt (already expanded)
	SystemPrompt string

	// Behavior
	MaxTurns int
	Search   bool
}

// CLIFlags holds the CLI flag values that can override settings.
type CLIFlags struct {
	Provider      string
	Tools         string
	ReadDirs      []string
	WriteDirs     []string
	ShellAllow    []string
	MCP           string
	SystemMessage string
	MaxTurns      int
	MaxTurnsSet   bool // true if --max-turns was explicitly set
	Search        bool
	Files         []string // files passed via -f flag, used for agent template expansion (e.g., {{.Files}})
}

// LoadAgent loads and validates an agent by name.
// Returns nil if agentName is empty.
func LoadAgent(agentName string, cfg *config.Config) (*agents.Agent, error) {
	if agentName == "" {
		return nil, nil
	}

	registry, err := agents.NewRegistry(agents.RegistryConfig{
		UseBuiltin:  cfg.Agents.UseBuiltin,
		SearchPaths: cfg.Agents.SearchPaths,
	})
	if err != nil {
		return nil, fmt.Errorf("create agent registry: %w", err)
	}

	agent, err := registry.Get(agentName)
	if err != nil {
		return nil, fmt.Errorf("load agent: %w", err)
	}

	if err := agent.Validate(); err != nil {
		return nil, fmt.Errorf("invalid agent: %w", err)
	}

	return agent, nil
}

// ResolveSettings merges config, agent, and CLI flags into final settings.
// Priority: CLI > agent > config
func ResolveSettings(cfg *config.Config, agent *agents.Agent, cli CLIFlags, configProvider, configModel, configInstructions string, configMaxTurns, defaultMaxTurns int) SessionSettings {
	s := SessionSettings{}

	// Provider/model: CLI > agent > config
	s.Provider = configProvider
	s.Model = configModel
	if agent != nil {
		if agent.Provider != "" {
			s.Provider = agent.Provider
		}
		if agent.Model != "" {
			s.Model = agent.Model
		}
	}
	// CLI provider flag is handled separately via applyProviderOverridesWithAgent

	// Tools: CLI > agent
	if cli.Tools != "" {
		s.Tools = cli.Tools
	} else if agent != nil {
		if agent.HasEnabledList() {
			s.Tools = strings.Join(agent.Tools.Enabled, ",")
		} else if agent.HasDisabledList() {
			allTools := tools.AllToolNames()
			enabledTools := agent.GetEnabledTools(allTools)
			s.Tools = strings.Join(enabledTools, ",")
		}
	}

	// Read/Write/Shell dirs: CLI > agent
	if len(cli.ReadDirs) > 0 {
		s.ReadDirs = cli.ReadDirs
	} else if agent != nil {
		s.ReadDirs = agent.Read.Dirs
	}

	if len(cli.WriteDirs) > 0 {
		s.WriteDirs = cli.WriteDirs
	}
	// Note: agents don't have write dirs currently

	if len(cli.ShellAllow) > 0 {
		s.ShellAllow = cli.ShellAllow
	} else if agent != nil {
		s.ShellAllow = agent.Shell.Allow
	}

	// Shell auto-run and scripts from agent only
	if agent != nil {
		s.ShellAutoRun = agent.Shell.AutoRun
		// Extract script commands from map (sorted for determinism)
		keys := make([]string, 0, len(agent.Shell.Scripts))
		for k := range agent.Shell.Scripts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s.Scripts = append(s.Scripts, agent.Shell.Scripts[k])
		}
	}

	// MCP: CLI > agent
	if cli.MCP != "" {
		s.MCP = cli.MCP
	} else if agent != nil {
		mcpServers := agent.GetMCPServerNames()
		if len(mcpServers) > 0 {
			s.MCP = strings.Join(mcpServers, ",")
		}
	}

	// System prompt: CLI > agent > config
	if cli.SystemMessage != "" {
		s.SystemPrompt = cli.SystemMessage
	} else if agent != nil && agent.SystemPrompt != "" {
		// Expand template variables
		templateCtx := agents.NewTemplateContext().WithFiles(cli.Files)
		if agents.IsBuiltinAgent(agent.Name) {
			if resourceDir, err := agents.ExtractBuiltinResources(agent.Name); err == nil {
				templateCtx = templateCtx.WithResourceDir(resourceDir)
			}
		}
		s.SystemPrompt = agents.ExpandTemplate(agent.SystemPrompt, templateCtx)
	} else {
		s.SystemPrompt = configInstructions
	}

	// Max turns: CLI (if set) > agent > config > default
	if cli.MaxTurnsSet {
		s.MaxTurns = cli.MaxTurns
	} else if agent != nil && agent.MaxTurns > 0 {
		s.MaxTurns = agent.MaxTurns
	} else if configMaxTurns > 0 {
		s.MaxTurns = configMaxTurns
	} else {
		s.MaxTurns = defaultMaxTurns
	}

	// Search: CLI or agent enables it
	s.Search = cli.Search || (agent != nil && agent.Search)

	return s
}

// SetupToolManager creates and configures a ToolManager from settings.
// Returns nil if no tools are enabled.
func (s *SessionSettings) SetupToolManager(cfg *config.Config, engine *llm.Engine) (*tools.ToolManager, error) {
	if s.Tools == "" {
		return nil, nil
	}

	toolConfig := buildToolConfig(s.Tools, s.ReadDirs, s.WriteDirs, s.ShellAllow, cfg)
	if s.ShellAutoRun {
		toolConfig.ShellAutoRun = true
	}
	if len(s.Scripts) > 0 {
		toolConfig.ScriptCommands = append(toolConfig.ScriptCommands, s.Scripts...)
	}

	if errs := toolConfig.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("invalid tool config: %v", errs[0])
	}

	toolMgr, err := tools.NewToolManager(&toolConfig, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize tools: %w", err)
	}

	toolMgr.SetupEngine(engine)
	return toolMgr, nil
}

// WireSpawnAgentRunner sets up the spawn_agent runner if the tool is enabled.
// This should be called after SetupToolManager.
func WireSpawnAgentRunner(cfg *config.Config, toolMgr *tools.ToolManager, yoloMode bool) error {
	if toolMgr == nil {
		return nil
	}
	spawnTool := toolMgr.GetSpawnAgentTool()
	if spawnTool == nil {
		return nil
	}
	runner, err := NewSpawnAgentRunner(cfg, yoloMode)
	if err != nil {
		return fmt.Errorf("setup spawn_agent: %w", err)
	}
	spawnTool.SetRunner(runner)
	return nil
}
