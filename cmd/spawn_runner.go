package cmd

import (
	"context"
	"errors"
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

var errSpawnAgentRunnerDraining = errors.New("spawn agent runner is shutting down")

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
	runMu             sync.Mutex
	draining          bool
	wg                sync.WaitGroup // tracks admitted agent runs so callers can drain before closing the store
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

// Wait permanently prevents new agent runs from starting, then blocks until
// all admitted runs have completed. The runner cannot be reused after Wait.
// Call this before closing the session store.
func (r *SpawnAgentRunner) Wait() {
	r.runMu.Lock()
	r.draining = true
	r.runMu.Unlock()
	r.wg.Wait()
}

// beginRun serializes run admission with Wait. A bare WaitGroup permits Add to
// race with Wait while its count is zero, which is both invalid and capable of
// letting a late tool execution outlive the session store.
func (r *SpawnAgentRunner) beginRun() bool {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if r.draining {
		return false
	}
	r.wg.Add(1)
	return true
}

func (r *SpawnAgentRunner) endRun() {
	r.wg.Done()
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
	request := runpkg.ChildRunRequest{
		Kind:          runpkg.ChildRunSpawnAgent,
		AgentName:     agentName,
		Prompt:        prompt,
		ModelOverride: opts.ModelOverride,
		Depth:         depth,
	}
	return r.buildChildExecutionRequest(ctx, request, childSessionID, search)
}

func (r *SpawnAgentRunner) buildChildExecutionRequest(ctx context.Context, request runpkg.ChildRunRequest, childSessionID string, search bool) runpkg.Request {
	parentSessionID := strings.TrimSpace(request.ParentSessionID)
	if parentSessionID == "" {
		parentSessionID = r.parentSessionID
		if contextSessionID := llm.SessionIDFromContext(ctx); contextSessionID != "" {
			parentSessionID = contextSessionID
		}
	}
	baseDir := strings.TrimSpace(request.BaseDir)
	if baseDir == "" {
		baseDir = r.currentBaseDir()
	}
	approvalRole := "parent_agent_task"
	if request.Kind == runpkg.ChildRunIsolatedSkill {
		approvalRole = "user_skill_activation"
	}
	sessionName := fmt.Sprintf("@%s: %s", request.AgentName, session.TruncateSummary(request.Prompt))
	if request.Skill != nil {
		sessionName = fmt.Sprintf("/%s @%s: %s", request.Skill.Name, request.AgentName, session.TruncateSummary(request.Prompt))
	}
	return runpkg.Request{
		Platform:                 runpkg.PlatformConsole,
		AgentName:                request.AgentName,
		Prompt:                   request.Prompt,
		SessionID:                childSessionID,
		SessionName:              sessionName,
		Persist:                  r.store != nil,
		Model:                    strings.TrimSpace(request.ModelOverride),
		Cwd:                      baseDir,
		Search:                   &search,
		ParentSessionID:          parentSessionID,
		IsSubagent:               true,
		Depth:                    request.Depth,
		ApprovalRole:             approvalRole,
		ApprovalTranscriptPrefix: subagentApprovalTranscriptPrefix(ctx),
		ChildSkill:               request.Skill,
	}
}

// ResolveSpawnAgent validates one exact registry lookup key using the same
// registry and definition validation path as execution.
func (r *SpawnAgentRunner) ResolveSpawnAgent(name string) error {
	_, err := r.resolveSpawnAgent(name)
	return err
}

// ListSpawnAgentNames returns only exact registry keys whose definitions can be
// loaded and validated by this runner. Invalid definitions are omitted.
func (r *SpawnAgentRunner) ListSpawnAgentNames() ([]string, error) {
	if r == nil || r.registry == nil {
		return nil, errors.New("spawn agent registry is not configured")
	}
	names, err := r.registry.ListNames()
	if err != nil {
		return nil, fmt.Errorf("list agent registry: %w", err)
	}
	valid := make([]string, 0, len(names))
	for _, name := range names {
		if _, err := r.resolveSpawnAgent(name); err == nil {
			valid = append(valid, name)
		}
	}
	return valid, nil
}

func (r *SpawnAgentRunner) resolveSpawnAgent(name string) (*agents.Agent, error) {
	if r == nil || r.registry == nil {
		return nil, errors.New("spawn agent registry is not configured")
	}
	if !agents.IsLookupName(name) {
		return nil, fmt.Errorf("invalid agent lookup name %q", name)
	}
	agent, err := r.registry.Get(name)
	if err != nil {
		return nil, fmt.Errorf("load agent '%s': %w", name, err)
	}
	if err := agent.Validate(); err != nil {
		return nil, fmt.Errorf("invalid agent '%s': %w", name, err)
	}
	return agent, nil
}

// RunChild executes the generic child-runtime contract used by direct isolated
// skills. spawn_agent is an adapter over the same internal path.
func (r *SpawnAgentRunner) RunChild(ctx context.Context, request runpkg.ChildRunRequest, callback runpkg.ChildRunEventCallback) (runpkg.ChildRunResult, error) {
	return r.runChildInternal(ctx, request, callback)
}

// runAgentInternal adapts the model-facing spawn_agent contract to ChildRunner.
func (r *SpawnAgentRunner) runAgentInternal(ctx context.Context, agentName string, prompt string, depth int,
	callID string, cb tools.SubagentEventCallback, opts tools.SpawnAgentRunOptions) (tools.SpawnAgentRunResult, error) {
	request := runpkg.ChildRunRequest{
		Kind:          runpkg.ChildRunSpawnAgent,
		RunID:         callID,
		AgentName:     agentName,
		Prompt:        prompt,
		ModelOverride: opts.ModelOverride,
		Depth:         depth,
	}
	var callback runpkg.ChildRunEventCallback
	if cb != nil {
		callback = func(runID string, event tools.SubagentEvent) { cb(runID, event) }
	}
	result, err := r.runChildInternal(ctx, request, callback)
	return tools.SpawnAgentRunResult{Output: result.Output, SessionID: result.ChildSessionID}, err
}

func (r *SpawnAgentRunner) runChildInternal(ctx context.Context, request runpkg.ChildRunRequest, callback runpkg.ChildRunEventCallback) (runpkg.ChildRunResult, error) {
	startedAt := time.Now()
	emptyResult := runpkg.ChildRunResult{RunID: request.RunID, StartedAt: startedAt}
	if !r.beginRun() {
		return emptyResult, errSpawnAgentRunnerDraining
	}
	defer r.endRun()

	agentName := strings.TrimSpace(request.AgentName)
	if agentName == "" {
		agentName = "developer"
	}
	agent, err := r.resolveSpawnAgent(agentName)
	if err != nil {
		return emptyResult, err
	}
	if strings.TrimSpace(request.ModelOverride) != "" {
		agentCopy := *agent
		agentCopy.Model = strings.TrimSpace(request.ModelOverride)
		agent = &agentCopy
		if err := agent.Validate(); err != nil {
			return emptyResult, fmt.Errorf("invalid agent '%s': %w", agentName, err)
		}
	}
	request.AgentName = agentName

	childSessionID := strings.TrimSpace(request.ChildSessionID)
	if childSessionID == "" {
		childSessionID = session.NewID()
	}
	providerName, modelName := r.previewAgentProviderModel(agent)
	sinkCallback := tools.SubagentEventCallback(nil)
	if callback != nil {
		sinkCallback = func(runID string, event tools.SubagentEvent) { callback(runID, event) }
	}
	sink := &spawnRunSink{callID: request.RunID, cb: sinkCallback, provider: providerName, model: modelName}
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
	executionRequest := r.buildChildExecutionRequest(ctx, request, childSessionID, search)
	result, err := runner.Run(ctx, executionRequest, sink)

	output, completionErr := completeChildAgent(agent, result, sink.Output(), executionRequest.Cwd)
	if completionErr != nil {
		r.warn("agent %q on_complete failed: %v", agentName, completionErr)
	}
	completedAt := time.Now()
	childResult := runpkg.ChildRunResult{
		RunID:          request.RunID,
		ChildSessionID: childSessionID,
		Output:         output,
		Provider:       providerName,
		Model:          modelName,
		StartedAt:      startedAt,
		CompletedAt:    completedAt,
	}
	if r.store != nil {
		status := session.StatusComplete
		if errors.Is(err, context.Canceled) {
			status = session.StatusInterrupted
		} else if err != nil {
			status = session.StatusError
		}
		dbCtx, dbCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if statusErr := safeStoreOp(func() error { return r.store.UpdateStatus(dbCtx, childSessionID, status) }); statusErr != nil {
			r.warn("session UpdateStatus failed: %v", statusErr)
		}
		dbCancel()
	}
	return childResult, err
}

// completeChildAgent resolves the agent's semantic result before presentation.
// Agents with an output tool use that captured value as their required return
// channel; streamed prose is only a fallback for ordinary agents.
func completeChildAgent(agent *agents.Agent, result runpkg.Result, streamedOutput, baseDir string) (string, error) {
	output := streamedOutput
	if output == "" {
		output = result.Response
	}
	if agent != nil && agent.OutputTool.IsConfigured() && result.Engine != nil {
		if registered, ok := result.Engine.Tools().Get(agent.OutputTool.Name); ok {
			if outputTool, ok := registered.(*tools.SetOutputTool); ok && outputTool.Captured() {
				output = outputTool.Value()
			}
		}
	}
	if agent == nil || strings.TrimSpace(agent.OnComplete) == "" || output == "" {
		return output, nil
	}
	_, err := runOnCompleteCaptureInDir(agent.OnComplete, output, baseDir)
	return output, err
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
	toolMgr.Registry.SetPlanStore(r.store)
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
