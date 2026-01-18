package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

// SpawnAgentRunner implements the tools.SpawnAgentRunner interface.
// It loads and runs sub-agents for the spawn_agent tool.
type SpawnAgentRunner struct {
	cfg      *config.Config
	registry *agents.Registry
	yoloMode bool // Auto-approve all tool operations in sub-agents
}

// NewSpawnAgentRunner creates a new SpawnAgentRunner.
func NewSpawnAgentRunner(cfg *config.Config, yoloMode bool) (*SpawnAgentRunner, error) {
	registry, err := agents.NewRegistry(agents.RegistryConfig{
		UseBuiltin:  cfg.Agents.UseBuiltin,
		SearchPaths: cfg.Agents.SearchPaths,
	})
	if err != nil {
		return nil, fmt.Errorf("create agent registry: %w", err)
	}

	return &SpawnAgentRunner{
		cfg:      cfg,
		registry: registry,
		yoloMode: yoloMode,
	}, nil
}

// RunAgent loads and runs a sub-agent with the given prompt.
// It returns the text output from the agent.
func (r *SpawnAgentRunner) RunAgent(ctx context.Context, agentName string, prompt string, depth int) (string, error) {
	// Load the agent
	agent, err := r.registry.Get(agentName)
	if err != nil {
		return "", fmt.Errorf("load agent '%s': %w", agentName, err)
	}

	if err := agent.Validate(); err != nil {
		return "", fmt.Errorf("invalid agent '%s': %w", agentName, err)
	}

	// Create a copy of config for potential provider overrides
	cfg := r.cfg

	// Apply provider overrides from agent
	if agent.Provider != "" || agent.Model != "" {
		// Make a shallow copy to avoid modifying the original
		cfgCopy := *cfg
		cfg = &cfgCopy

		if agent.Provider != "" {
			cfg.DefaultProvider = agent.Provider
		}
		if agent.Model != "" {
			// Set model on the active provider config
			if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
				providerCfg.Model = agent.Model
			}
		}
	}

	// Create provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return "", fmt.Errorf("create provider: %w", err)
	}

	// Create engine with default tool registry
	engine := llm.NewEngine(provider, defaultToolRegistry(cfg))

	// Set up tools from agent config
	toolMgr, err := r.setupAgentTools(cfg, engine, agent, depth)
	if err != nil {
		return "", fmt.Errorf("setup tools: %w", err)
	}

	// Build system prompt
	systemPrompt := ""
	if agent.SystemPrompt != "" {
		templateCtx := agents.NewTemplateContext()
		if agents.IsBuiltinAgent(agent.Name) {
			if resourceDir, err := agents.ExtractBuiltinResources(agent.Name); err == nil {
				templateCtx = templateCtx.WithResourceDir(resourceDir)
			}
		}
		systemPrompt = agents.ExpandTemplate(agent.SystemPrompt, templateCtx)
	}

	// Build messages
	messages := []llm.Message{}
	if systemPrompt != "" {
		messages = append(messages, llm.SystemText(systemPrompt))
	}
	messages = append(messages, llm.UserText(prompt))

	// Determine max turns
	maxTurns := 20
	if agent.MaxTurns > 0 {
		maxTurns = agent.MaxTurns
	}

	// Build request
	req := llm.Request{
		Messages:          messages,
		Search:            agent.Search,
		ParallelToolCalls: true,
		MaxTurns:          maxTurns,
	}

	// Add tools if any
	if toolMgr != nil {
		allSpecs := engine.Tools().AllSpecs()
		// Filter out search tools unless search is enabled
		if !agent.Search {
			var filtered []llm.ToolSpec
			for _, spec := range allSpecs {
				if spec.Name != llm.WebSearchToolName && spec.Name != llm.ReadURLToolName {
					filtered = append(filtered, spec)
				}
			}
			req.Tools = filtered
		} else {
			req.Tools = allSpecs
		}
		req.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}

	// Run the agent and collect output
	return r.runAndCollect(ctx, engine, req)
}

// setupAgentTools sets up tools based on agent configuration.
func (r *SpawnAgentRunner) setupAgentTools(cfg *config.Config, engine *llm.Engine, agent *agents.Agent, depth int) (*tools.ToolManager, error) {
	// Determine which tools to enable
	var enabledTools string
	if agent.HasEnabledList() {
		enabledTools = strings.Join(agent.Tools.Enabled, ",")
	} else if agent.HasDisabledList() {
		allTools := tools.AllToolNames()
		enabled := agent.GetEnabledTools(allTools)
		enabledTools = strings.Join(enabled, ",")
	}

	if enabledTools == "" {
		return nil, nil
	}

	// Build tool config
	toolConfig := buildToolConfig(enabledTools, agent.Read.Dirs, nil, agent.Shell.Allow, cfg)
	if agent.Shell.AutoRun {
		toolConfig.ShellAutoRun = true
	}
	if len(agent.Shell.Scripts) > 0 {
		for _, script := range agent.Shell.Scripts {
			toolConfig.ScriptCommands = append(toolConfig.ScriptCommands, script)
		}
	}

	// Apply spawn config from agent (with depth tracking)
	toolConfig.Spawn = tools.SpawnConfig{
		MaxParallel:    agent.Spawn.MaxParallel,
		MaxDepth:       agent.Spawn.MaxDepth,
		DefaultTimeout: agent.Spawn.DefaultTimeout,
		AllowedAgents:  agent.Spawn.AllowedAgents,
	}
	if toolConfig.Spawn.MaxParallel <= 0 {
		toolConfig.Spawn.MaxParallel = 3
	}
	if toolConfig.Spawn.MaxDepth <= 0 {
		toolConfig.Spawn.MaxDepth = 2
	}
	if toolConfig.Spawn.DefaultTimeout <= 0 {
		toolConfig.Spawn.DefaultTimeout = 300
	}

	if errs := toolConfig.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("invalid tool config: %v", errs[0])
	}

	toolMgr, err := tools.NewToolManager(&toolConfig, cfg)
	if err != nil {
		return nil, err
	}

	// Set yolo mode for sub-agents (they inherit from parent)
	if r.yoloMode {
		toolMgr.ApprovalMgr.SetYoloMode(true)
	}

	toolMgr.SetupEngine(engine)

	// Wire up spawn_agent runner for nested agents (with incremented depth)
	if spawnTool := toolMgr.GetSpawnAgentTool(); spawnTool != nil {
		// Set the depth for this nested spawn tool
		spawnTool.SetDepth(depth)
		// Create a new runner with the same config
		childRunner := &SpawnAgentRunner{
			cfg:      r.cfg,
			registry: r.registry,
			yoloMode: r.yoloMode,
		}
		spawnTool.SetRunner(childRunner)
	}

	return toolMgr, nil
}

// runAndCollect runs the engine and collects text output.
func (r *SpawnAgentRunner) runAndCollect(ctx context.Context, engine *llm.Engine, req llm.Request) (string, error) {
	stream, err := engine.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var output strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		switch event.Type {
		case llm.EventTextDelta:
			output.WriteString(event.Text)
		case llm.EventError:
			if event.Err != nil {
				return output.String(), event.Err
			}
		}
	}

	return output.String(), nil
}
