package cmd

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// SpawnAgentRunner implements the tools.SpawnAgentRunner interface.
// It loads and runs sub-agents for the spawn_agent tool.
type SpawnAgentRunner struct {
	cfg               *config.Config
	registry          *agents.Registry
	yoloMode          bool // Auto-approve all tool operations in sub-agents
	parentApprovalMgr *tools.ApprovalManager
	store             session.Store // Session store for tracking subagent turns
	parentSessionID   string        // Parent session ID for child session linking
	warnFunc          func(format string, args ...any)
}

// NewSpawnAgentRunner creates a new SpawnAgentRunner.
// parentApprovalMgr enables sub-agents to inherit parent's session approvals and prompting.
func NewSpawnAgentRunner(cfg *config.Config, yoloMode bool, parentApprovalMgr *tools.ApprovalManager) (*SpawnAgentRunner, error) {
	return NewSpawnAgentRunnerWithStore(cfg, yoloMode, parentApprovalMgr, nil, "")
}

// NewSpawnAgentRunnerWithStore creates a new SpawnAgentRunner with session tracking.
// store is used to save subagent turns, parentSessionID links child sessions to parent.
func NewSpawnAgentRunnerWithStore(cfg *config.Config, yoloMode bool, parentApprovalMgr *tools.ApprovalManager, store session.Store, parentSessionID string) (*SpawnAgentRunner, error) {
	registry, err := agents.NewRegistry(agents.RegistryConfig{
		UseBuiltin:  cfg.Agents.UseBuiltin,
		SearchPaths: cfg.Agents.SearchPaths,
	})
	if err != nil {
		return nil, fmt.Errorf("create agent registry: %w", err)
	}

	// Apply agent preferences from config
	registry.SetPreferences(cfg.Agents.Preferences)

	return &SpawnAgentRunner{
		cfg:               cfg,
		registry:          registry,
		yoloMode:          yoloMode,
		parentApprovalMgr: parentApprovalMgr,
		store:             store,
		parentSessionID:   parentSessionID,
	}, nil
}

// SetWarnFunc sets a function to be called when non-fatal warnings occur
// (e.g., session persistence failures). If not set, warnings are logged via log.Printf.
func (r *SpawnAgentRunner) SetWarnFunc(fn func(format string, args ...any)) {
	r.warnFunc = fn
}

// warn logs a warning using warnFunc if set, otherwise uses log.Printf.
func (r *SpawnAgentRunner) warn(format string, args ...any) {
	if r.warnFunc != nil {
		r.warnFunc(format, args...)
	} else {
		log.Printf("Warning: "+format, args...)
	}
}

// RunAgent loads and runs a sub-agent with the given prompt.
// It returns the text output from the agent.
func (r *SpawnAgentRunner) RunAgent(ctx context.Context, agentName string, prompt string, depth int) (tools.SpawnAgentRunResult, error) {
	return r.runAgentInternal(ctx, agentName, prompt, depth, "", nil)
}

// RunAgentWithCallback loads and runs a sub-agent with an event callback for progress reporting.
func (r *SpawnAgentRunner) RunAgentWithCallback(ctx context.Context, agentName string, prompt string, depth int,
	callID string, cb tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	return r.runAgentInternal(ctx, agentName, prompt, depth, callID, cb)
}

// runAgentInternal is the shared implementation for running sub-agents.
func (r *SpawnAgentRunner) runAgentInternal(ctx context.Context, agentName string, prompt string, depth int,
	callID string, cb tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	emptyResult := tools.SpawnAgentRunResult{}

	// Load the agent
	agent, err := r.registry.Get(agentName)
	if err != nil {
		return emptyResult, fmt.Errorf("load agent '%s': %w", agentName, err)
	}

	if err := agent.Validate(); err != nil {
		return emptyResult, fmt.Errorf("invalid agent '%s': %w", agentName, err)
	}

	// Create a copy of config for potential provider overrides
	cfg := r.cfg

	// Apply provider overrides from agent
	if agent.Provider != "" || agent.Model != "" {
		// Deep copy to avoid modifying the original config (which may be shared
		// by other sub-agents or the parent). This is critical because ProviderConfig
		// contains pointer fields (UseNativeSearch, OAuthCreds) and slices (Models)
		// that would be shared in a shallow copy.
		cfgCopy := *cfg
		cfgCopy.Providers = make(map[string]config.ProviderConfig, len(cfg.Providers))
		for k, v := range cfg.Providers {
			// Deep copy slice fields
			if v.Models != nil {
				v.Models = append([]string(nil), v.Models...)
			}
			// Deep copy pointer fields
			if v.UseNativeSearch != nil {
				tmp := *v.UseNativeSearch
				v.UseNativeSearch = &tmp
			}
			if v.OAuthCreds != nil {
				credsCopy := *v.OAuthCreds
				v.OAuthCreds = &credsCopy
			}
			cfgCopy.Providers[k] = v
		}
		cfg = &cfgCopy

		if agent.Provider != "" {
			cfg.DefaultProvider = agent.Provider
		}
		if agent.Model != "" {
			// Set model on the active provider config
			// We must copy the provider config struct and reassign it to the map
			// to avoid mutating the original (GetActiveProviderConfig returns a
			// pointer to a copy, but we need to update the map entry).
			if providerCfg, ok := cfg.Providers[cfg.DefaultProvider]; ok {
				providerCfg.Model = agent.Model
				cfg.Providers[cfg.DefaultProvider] = providerCfg
			}
		}
	}

	// Create provider
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return emptyResult, fmt.Errorf("create provider: %w", err)
	}

	// Get provider name and model for session tracking
	providerName := cfg.DefaultProvider
	modelName := agent.Model
	if modelName == "" {
		if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
			modelName = providerCfg.Model
		}
	}

	// Create child session if store is available (before engine setup so nested agents can reference it)
	var childSessionID string
	if r.store != nil {
		childSession := &session.Session{
			ID:         session.NewID(),
			ParentID:   r.parentSessionID,
			IsSubagent: true,
			Provider:   providerName,
			Model:      modelName,
			Agent:      agentName,
			Summary:    fmt.Sprintf("@%s: %s", agentName, session.TruncateSummary(prompt)),
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
			Status:     session.StatusActive,
		}
		if cwd, err := os.Getwd(); err == nil {
			childSession.CWD = cwd
		}
		if err := safeStoreOp(func() error { return r.store.Create(ctx, childSession) }); err != nil {
			r.warn("session Create failed: %v", err)
		} else {
			childSessionID = childSession.ID

			// Save initial user prompt as first message
			userMsg := session.NewMessage(childSessionID, llm.UserText(prompt), -1)
			if err := safeStoreOp(func() error { return r.store.AddMessage(ctx, childSessionID, userMsg) }); err != nil {
				r.warn("session AddMessage failed: %v", err)
			}
		}
	}

	// Create engine with default tool registry
	engine := newEngine(provider, cfg)

	// Set up tools from agent config (pass child session ID for nested agents)
	toolMgr, err := r.setupAgentTools(cfg, engine, agent, depth, childSessionID)
	if err != nil {
		return emptyResult, fmt.Errorf("setup tools: %w", err)
	}

	// Set up callbacks to save messages incrementally (after tools setup)
	// Track when streaming starts for duration calculation
	streamStartTime := time.Now()
	if r.store != nil && childSessionID != "" {
		// Response callback saves assistant message immediately (before tool execution)
		// This ensures the message is persisted even if tool execution fails/crashes
		engine.SetResponseCompletedCallback(func(ctx context.Context, turnIndex int, assistantMsg llm.Message, metrics llm.TurnMetrics) error {
			sessionMsg := session.NewMessage(childSessionID, assistantMsg, -1)
			sessionMsg.DurationMs = time.Since(streamStartTime).Milliseconds()
			if err := r.store.AddMessage(ctx, childSessionID, sessionMsg); err != nil {
				r.warn("session AddMessage failed: %v", err)
			}
			return nil
		})

		// Turn callback saves tool result messages and updates metrics
		engine.SetTurnCompletedCallback(func(ctx context.Context, turnIndex int, turnMessages []llm.Message, metrics llm.TurnMetrics) error {
			for _, msg := range turnMessages {
				sessionMsg := session.NewMessage(childSessionID, msg, -1)
				// Set duration for assistant messages (when responseCallback didn't run)
				if msg.Role == llm.RoleAssistant {
					sessionMsg.DurationMs = time.Since(streamStartTime).Milliseconds()
				}
				if err := r.store.AddMessage(ctx, childSessionID, sessionMsg); err != nil {
					r.warn("session AddMessage failed: %v", err)
				}
			}
			if err := r.store.UpdateMetrics(ctx, childSessionID, 1, metrics.ToolCalls, metrics.InputTokens, metrics.OutputTokens, metrics.CachedInputTokens); err != nil {
				r.warn("session UpdateMetrics failed: %v", err)
			}
			return nil
		})
	}

	// Build system prompt
	systemPrompt := ""
	if agent.SystemPrompt != "" {
		templateCtx, includeBaseDir, err := agentPromptTemplateContextAndBaseDir(agent, nil)
		if err != nil {
			return tools.SpawnAgentRunResult{}, fmt.Errorf("prepare agent system prompt context: %w", err)
		}
		systemPrompt, err = expandSystemPromptWithIncludes(agent.SystemPrompt, templateCtx, includeBaseDir)
		if err != nil {
			return tools.SpawnAgentRunResult{}, fmt.Errorf("expand agent system prompt: %w", err)
		}

		// Append project instructions if agent requests them
		if agent.ShouldLoadProjectInstructions() {
			if projectInstructions := agents.DiscoverProjectInstructions(); projectInstructions != "" {
				systemPrompt += "\n\n---\n\n" + projectInstructions
			}
		}
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
		SessionID:         childSessionID,
		Messages:          messages,
		Search:            agent.Search,
		ParallelToolCalls: true,
		MaxTurns:          maxTurns,
	}

	// Add tools if any
	if toolMgr != nil {
		if specs := llm.ToolSpecsForRequest(engine.Tools(), agent.Search); len(specs) > 0 {
			req.Tools = specs
			req.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
		}
	}

	// Run the agent and collect output
	output, err := r.runAndCollectWithCallback(ctx, engine, req, callID, cb, providerName, modelName)
	if err != nil {
		// Update session status on error
		if r.store != nil && childSessionID != "" {
			if statusErr := r.store.UpdateStatus(ctx, childSessionID, session.StatusError); statusErr != nil {
				r.warn("session UpdateStatus failed: %v", statusErr)
			}
		}
		return tools.SpawnAgentRunResult{Output: output, SessionID: childSessionID}, err
	}

	// Update session status on completion
	if r.store != nil && childSessionID != "" {
		if statusErr := r.store.UpdateStatus(ctx, childSessionID, session.StatusComplete); statusErr != nil {
			r.warn("session UpdateStatus failed: %v", statusErr)
		}
	}

	return tools.SpawnAgentRunResult{Output: output, SessionID: childSessionID}, nil
}

// setupAgentTools sets up tools based on agent configuration.
// childSessionID is the session ID for this agent run, used as parent for nested agents.
func (r *SpawnAgentRunner) setupAgentTools(cfg *config.Config, engine *llm.Engine, agent *agents.Agent, depth int, childSessionID string) (*tools.ToolManager, error) {
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
	toolConfig.AgentDir = agent.SourcePath
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
	wireImageRecorder(toolMgr.Registry, agent.Name, childSessionID)

	// Register any custom script-backed tools declared in agent.yaml
	if len(agent.Tools.Custom) > 0 {
		if err := toolMgr.Registry.RegisterCustomTools(agent.Tools.Custom, agent.SourcePath); err != nil {
			return nil, fmt.Errorf("custom tools: %w", err)
		}
	}

	// Set yolo mode for sub-agents (they inherit from parent)
	if r.yoloMode {
		toolMgr.ApprovalMgr.SetYoloMode(true)
	}

	// Inherit parent's session approvals and prompting capability
	if r.parentApprovalMgr != nil {
		if err := toolMgr.ApprovalMgr.SetParent(r.parentApprovalMgr); err != nil {
			return nil, fmt.Errorf("failed to set parent approval manager: %w", err)
		}
	}

	toolMgr.SetupEngine(engine)

	// Wire up spawn_agent runner for nested agents (with incremented depth)
	if spawnTool := toolMgr.GetSpawnAgentTool(); spawnTool != nil {
		// Set the depth for this nested spawn tool
		spawnTool.SetDepth(depth)
		// Create a new runner - this sub-agent's ApprovalMgr becomes the parent for nested agents
		// Pass store and childSessionID so nested agents can track their sessions
		childRunner := &SpawnAgentRunner{
			cfg:               r.cfg,
			registry:          r.registry,
			yoloMode:          r.yoloMode,
			parentApprovalMgr: toolMgr.ApprovalMgr,
			store:             r.store,
			parentSessionID:   childSessionID, // This agent's session becomes parent for nested agents
			warnFunc:          r.warnFunc,
		}
		spawnTool.SetRunner(childRunner)
	}

	return toolMgr, nil
}

// runAndCollectWithCallback runs the engine and collects text output, optionally forwarding events.
func (r *SpawnAgentRunner) runAndCollectWithCallback(
	ctx context.Context, engine *llm.Engine, req llm.Request,
	callID string, cb tools.SubagentEventCallback,
	providerName, modelName string) (string, error) {
	stream, err := engine.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	// Send init event with provider/model info
	if cb != nil && callID != "" {
		cb(callID, tools.SubagentEvent{
			Type:     tools.SubagentEventInit,
			Provider: providerName,
			Model:    modelName,
		})
	}

	var output strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Send done event before returning on stream error
			if cb != nil && callID != "" {
				cb(callID, tools.SubagentEvent{Type: tools.SubagentEventDone})
			}
			// Return partial output alongside the error to aid debugging.
			// The caller can check output.String() for any content collected before the failure.
			return output.String(), err
		}

		switch event.Type {
		case llm.EventTextDelta:
			output.WriteString(event.Text)
			if cb != nil && callID != "" {
				cb(callID, tools.SubagentEvent{Type: tools.SubagentEventText, Text: event.Text})
			}
		case llm.EventToolExecStart:
			if cb != nil && callID != "" {
				cb(callID, tools.SubagentEvent{
					Type:     tools.SubagentEventToolStart,
					ToolName: event.ToolName,
					ToolInfo: event.ToolInfo,
				})
			}
		case llm.EventToolExecEnd:
			if cb != nil && callID != "" {
				cb(callID, tools.SubagentEvent{
					Type:     tools.SubagentEventToolEnd,
					ToolName: event.ToolName,
					Diffs:    event.ToolDiffs,
					Images:   event.ToolImages,
					Success:  event.ToolSuccess,
				})
			}
		case llm.EventPhase:
			if cb != nil && callID != "" {
				cb(callID, tools.SubagentEvent{Type: tools.SubagentEventPhase, Phase: event.Text})
			}
		case llm.EventUsage:
			if cb != nil && callID != "" && event.Use != nil {
				cb(callID, tools.SubagentEvent{
					Type:         tools.SubagentEventUsage,
					InputTokens:  event.Use.InputTokens,
					OutputTokens: event.Use.OutputTokens,
				})
			}
		case llm.EventError:
			if event.Err != nil {
				if cb != nil && callID != "" {
					cb(callID, tools.SubagentEvent{Type: tools.SubagentEventDone})
				}
				return output.String(), event.Err
			}
		}
	}

	// Send done event
	if cb != nil && callID != "" {
		cb(callID, tools.SubagentEvent{Type: tools.SubagentEventDone})
	}

	return output.String(), nil
}

// safeStoreOp wraps a store operation with panic recovery so that panics in
// best-effort session tracking never crash the program.
func safeStoreOp(op func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return op()
}
