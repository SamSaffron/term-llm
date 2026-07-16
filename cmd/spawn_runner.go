package cmd

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
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
	parentSessionID   string        // Fallback parent session ID when execution context has none
	parentBaseDir     string        // Fallback BaseDir for legacy callers
	parentBaseDirFunc func() string // Returns the parent's current per-session BaseDir
	warnFunc          func(format string, args ...any)
	wg                *sync.WaitGroup // tracks in-flight agent runs so callers can drain before closing the store
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
		wg:                &sync.WaitGroup{},
	}, nil
}

// SetBaseDir sets the fallback per-session BaseDir inherited by spawned agents.
func (r *SpawnAgentRunner) SetBaseDir(dir string) {
	if r != nil {
		r.parentBaseDir = strings.TrimSpace(dir)
	}
}

// SetBaseDirFunc sets a resolver for the parent's current BaseDir. Interactive
// sessions can switch worktrees after the runner is wired, so a copied path is
// not sufficient. It must be configured before the runner is installed on a
// SpawnAgentTool; the resolver itself must be safe for concurrent calls.
func (r *SpawnAgentRunner) SetBaseDirFunc(fn func() string) {
	if r != nil {
		r.parentBaseDirFunc = fn
	}
}

func (r *SpawnAgentRunner) currentBaseDir() string {
	if r != nil && r.parentBaseDirFunc != nil {
		if dir := strings.TrimSpace(r.parentBaseDirFunc()); dir != "" {
			return dir
		}
	}
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r.parentBaseDir)
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

// Wait blocks until all in-flight agent runs have completed.
// Call this before closing the session store to avoid use-after-close errors.
func (r *SpawnAgentRunner) Wait() {
	r.wg.Wait()
}

// RunAgent loads and runs a sub-agent with the given prompt.
// It returns the text output from the agent.
func (r *SpawnAgentRunner) RunAgent(ctx context.Context, agentName string, prompt string, depth int) (tools.SpawnAgentRunResult, error) {
	return r.runAgentInternal(ctx, agentName, prompt, depth, "", nil, tools.SpawnAgentRunOptions{})
}

// RunAgentWithOptions loads and runs a sub-agent with call-specific overrides.
func (r *SpawnAgentRunner) RunAgentWithOptions(ctx context.Context, agentName string, prompt string, depth int, opts tools.SpawnAgentRunOptions) (tools.SpawnAgentRunResult, error) {
	return r.runAgentInternal(ctx, agentName, prompt, depth, "", nil, opts)
}

// RunAgentWithCallback loads and runs a sub-agent with an event callback for progress reporting.
func (r *SpawnAgentRunner) RunAgentWithCallback(ctx context.Context, agentName string, prompt string, depth int,
	callID string, cb tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	return r.runAgentInternal(ctx, agentName, prompt, depth, callID, cb, tools.SpawnAgentRunOptions{})
}

// RunAgentWithCallbackAndOptions loads and runs a sub-agent with an event callback and call-specific overrides.
func (r *SpawnAgentRunner) RunAgentWithCallbackAndOptions(ctx context.Context, agentName string, prompt string, depth int,
	callID string, cb tools.SubagentEventCallback, opts tools.SpawnAgentRunOptions) (tools.SpawnAgentRunResult, error) {
	return r.runAgentInternal(ctx, agentName, prompt, depth, callID, cb, opts)
}

func (r *SpawnAgentRunner) buildRunRequest(ctx context.Context, agentName, prompt, childSessionID string, depth int, search bool, opts tools.SpawnAgentRunOptions) runpkg.Request {
	parentSessionID := r.parentSessionID
	if contextSessionID := llm.SessionIDFromContext(ctx); contextSessionID != "" {
		parentSessionID = contextSessionID
	}
	return runpkg.Request{
		Platform:                 runpkg.PlatformConsole,
		AgentName:                agentName,
		Prompt:                   prompt,
		SessionID:                childSessionID,
		SessionName:              fmt.Sprintf("@%s: %s", agentName, session.TruncateSummary(prompt)),
		Persist:                  r.store != nil,
		Model:                    strings.TrimSpace(opts.ModelOverride),
		Cwd:                      r.currentBaseDir(),
		Search:                   &search,
		ParentSessionID:          parentSessionID,
		IsSubagent:               true,
		Depth:                    depth,
		ApprovalRole:             "parent_agent_task",
		ApprovalTranscriptPrefix: subagentApprovalTranscriptPrefix(ctx),
	}
}

// runAgentInternal is the shared implementation for running sub-agents.
func (r *SpawnAgentRunner) runAgentInternal(ctx context.Context, agentName string, prompt string, depth int,
	callID string, cb tools.SubagentEventCallback, opts tools.SpawnAgentRunOptions) (tools.SpawnAgentRunResult, error) {
	r.wg.Add(1)
	defer r.wg.Done()

	emptyResult := tools.SpawnAgentRunResult{}

	agent, err := r.registry.Get(agentName)
	if err != nil {
		return emptyResult, fmt.Errorf("load agent '%s': %w", agentName, err)
	}
	if strings.TrimSpace(opts.ModelOverride) != "" {
		agentCopy := *agent
		agentCopy.Model = strings.TrimSpace(opts.ModelOverride)
		agent = &agentCopy
	}
	if err := agent.Validate(); err != nil {
		return emptyResult, fmt.Errorf("invalid agent '%s': %w", agentName, err)
	}

	childSessionID := session.NewID()
	providerName, modelName := r.previewAgentProviderModel(agent)
	sink := &spawnRunSink{callID: callID, cb: cb, provider: providerName, model: modelName}
	sink.Start()
	defer sink.Done()

	search := agent.Search
	runner := newCmdRunner(r.cfg, cmdRunnerOptions{
		ConfigSet:         true,
		Yolo:              r.yoloMode,
		DefaultMaxTurns:   20,
		ErrWriter:         io.Discard,
		Store:             r.store,
		ParentApprovalMgr: r.parentApprovalMgr,
	})
	result, err := runner.Run(ctx, r.buildRunRequest(ctx, agentName, prompt, childSessionID, depth, search, opts), sink)

	output := sink.Output()
	if output == "" {
		output = result.Response
	}
	if r.store != nil {
		status := session.StatusComplete
		if err != nil {
			status = session.StatusError
		}
		dbCtx, dbCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if statusErr := safeStoreOp(func() error { return r.store.UpdateStatus(dbCtx, childSessionID, status) }); statusErr != nil {
			r.warn("session UpdateStatus failed: %v", statusErr)
		}
		dbCancel()
	}
	if err != nil {
		return tools.SpawnAgentRunResult{Output: output, SessionID: childSessionID}, err
	}
	return tools.SpawnAgentRunResult{Output: output, SessionID: childSessionID}, nil
}

func (r *SpawnAgentRunner) previewAgentProviderModel(agent *agents.Agent) (string, string) {
	cfg := cloneConfigForServeJob(r.cfg)
	if agent != nil {
		_ = applyProviderOverridesWithAgent(cfg, "", "", "", agent.Provider, agent.Model)
	} else {
		cfg.ApplyOverrides(cfg.Ask.Provider, cfg.Ask.Model)
	}
	return strings.TrimSpace(cfg.DefaultProvider), strings.TrimSpace(activeModel(cfg))
}

type spawnRunSink struct {
	callID   string
	cb       tools.SubagentEventCallback
	provider string
	model    string

	mu       sync.Mutex
	output   strings.Builder
	started  bool
	doneSent bool
}

func (s *spawnRunSink) Start() {
	if s == nil || s.cb == nil || s.callID == "" {
		return
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	provider := s.provider
	model := s.model
	s.mu.Unlock()
	s.cb(s.callID, tools.SubagentEvent{Type: tools.SubagentEventInit, Provider: provider, Model: model})
}

func (s *spawnRunSink) Done() {
	if s == nil || s.cb == nil || s.callID == "" {
		return
	}
	s.mu.Lock()
	if s.doneSent {
		s.mu.Unlock()
		return
	}
	s.doneSent = true
	s.mu.Unlock()
	s.cb(s.callID, tools.SubagentEvent{Type: tools.SubagentEventDone})
}

func (s *spawnRunSink) Output() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output.String()
}

func (s *spawnRunSink) Event(event llm.Event) {
	if s == nil {
		return
	}
	s.Start()
	s.mu.Lock()
	if event.Type == llm.EventTextDelta {
		s.output.WriteString(event.Text)
	}
	s.mu.Unlock()
	if s.cb == nil || s.callID == "" {
		return
	}
	subagentEvent := subagentEventFromLLM(event)
	if subagentEvent.Type == "" {
		return
	}
	s.cb(s.callID, subagentEvent)
}

func (s *spawnRunSink) GuardianEvent(event tools.GuardianEvent) {
	if s == nil || s.cb == nil || s.callID == "" {
		return
	}
	s.Start()
	s.cb(s.callID, tools.SubagentEvent{Type: tools.SubagentEventGuardian, ToolCallID: event.ToolCallID, Guardian: &event})
}

func subagentEventFromLLM(event llm.Event) tools.SubagentEvent {
	switch event.Type {
	case llm.EventTextDelta:
		return tools.SubagentEvent{Type: tools.SubagentEventText, Text: event.Text}
	case llm.EventToolExecStart:
		return tools.SubagentEvent{Type: tools.SubagentEventToolStart, ToolCallID: event.ToolCallID, ToolName: event.ToolName, ToolInfo: event.ToolInfo, ToolArgs: event.ToolArgs}
	case llm.EventToolExecEnd:
		return tools.SubagentEvent{Type: tools.SubagentEventToolEnd, ToolCallID: event.ToolCallID, ToolName: event.ToolName, ToolOutput: event.ToolOutput, Diffs: event.ToolDiffs, Images: event.ToolImages, Success: event.ToolSuccess}
	case llm.EventPhase:
		return tools.SubagentEvent{Type: tools.SubagentEventPhase, Phase: event.Text}
	case llm.EventUsage:
		if event.Use != nil {
			return tools.SubagentEvent{Type: tools.SubagentEventUsage, InputTokens: event.Use.InputTokens, OutputTokens: event.Use.OutputTokens}
		}
	}
	return tools.SubagentEvent{}
}

// setupAgentTools keeps the historical spawn-agent tool wiring path available for
// focused tests while delegating to the shared SessionSettings tool setup.
func (r *SpawnAgentRunner) setupAgentTools(cfg *config.Config, engine *llm.Engine, agent *agents.Agent, depth int, childSessionID string) (*tools.ToolManager, error) {
	baseDir := r.currentBaseDir()
	settings, err := ResolveSettingsInDir(cfg, agent, CLIFlags{}, cfg.Ask.Provider, cfg.Ask.Model, cfg.Ask.Instructions, cfg.Ask.MaxTurns, 20, baseDir)
	if err != nil {
		return nil, err
	}
	settings.SessionID = childSessionID
	settings.Provider = strings.TrimSpace(cfg.DefaultProvider)
	settings.Model = strings.TrimSpace(activeModel(cfg))
	if baseDir != "" {
		settings.BaseDir = baseDir
		settings.ReadDirs = append(settings.ReadDirs, baseDir)
		settings.WriteDirs = append(settings.WriteDirs, baseDir)
		settings.ShellWorkingDir = baseDir
	}
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil || toolMgr == nil {
		return toolMgr, err
	}
	if r.yoloMode && r.parentApprovalMgr == nil {
		toolMgr.ApprovalMgr.SetYoloMode(true)
	}
	if r.parentApprovalMgr != nil {
		if err := toolMgr.ApprovalMgr.SetParent(r.parentApprovalMgr); err != nil {
			return nil, fmt.Errorf("failed to set parent approval manager: %w", err)
		}
	}
	_, err = WireSpawnAgentRunnerWithStoreAndDepth(cfg, toolMgr, r.yoloMode, r.store, childSessionID, depth)
	if err != nil {
		return nil, err
	}
	return toolMgr, nil
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

const maxSubagentParentApprovalEntries = 12

func subagentApprovalTranscriptPrefix(ctx context.Context) []llm.Message {
	parent := llm.ApprovalTranscriptFromContext(ctx)
	if len(parent) == 0 {
		return nil
	}
	if len(parent) > maxSubagentParentApprovalEntries {
		parent = parent[len(parent)-maxSubagentParentApprovalEntries:]
	}
	prefix := make([]llm.Message, 0, len(parent))
	for _, msg := range parent {
		copyMsg := msg
		if copyMsg.ApprovalRole == "" {
			switch copyMsg.Role {
			case llm.RoleUser:
				copyMsg.ApprovalRole = "parent_user"
			case llm.RoleAssistant:
				copyMsg.ApprovalRole = "parent_assistant"
			case llm.RoleTool:
				copyMsg.ApprovalRole = "parent_tool"
			default:
				copyMsg.ApprovalRole = "parent_" + string(copyMsg.Role)
			}
		}
		prefix = append(prefix, copyMsg)
	}
	return prefix
}
