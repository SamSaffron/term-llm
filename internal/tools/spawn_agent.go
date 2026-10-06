package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// SpawnAgentArgs are the arguments for the spawn_agent tool.
type SpawnAgentArgs struct {
	AgentName      string `json:"agent_name"`                 // Required: name of the agent to spawn
	Prompt         string `json:"prompt"`                     // Required: task/prompt for the sub-agent
	Timeout        int    `json:"timeout,omitempty"`          // Deprecated alias for wait (seconds)
	Wait           *int   `json:"wait,omitempty"`             // Time to wait, not a child deadline
	Model          string `json:"model,omitempty"`            // Optional: exact provider:model override
	CWD            string `json:"cwd,omitempty"`              // Optional: existing child working directory
	NotifyWhenDone bool   `json:"notify_when_done,omitempty"` // Reactivate parent session after completion
}

// Interventions carry a delivery disposition because queue acceptance is not
// proof of consumption. A child can finish, fail, or be cancelled between the
// moment a correction is accepted and the moment the agent could act on it.
const (
	// InterventionQueued means accepted by the child engine but not yet observed
	// entering the conversation.
	InterventionQueued = "queued"
	// InterventionConsumed means the child engine committed it into the run.
	InterventionConsumed = "consumed"
	// InterventionUndelivered means the run ended before the child could act.
	InterventionUndelivered = "undelivered"
	// InterventionMixed means some corrections were consumed and some were not.
	InterventionMixed = "mixed"
)

// SpawnAgentResult is the result returned by spawn_agent.
type SpawnAgentResult struct {
	AgentName    string    `json:"agent_name"`
	Output       string    `json:"output,omitempty"`
	Error        string    `json:"error,omitempty"`
	Type         string    `json:"type,omitempty"` // Error type for structured handling
	Duration     int64     `json:"duration_ms,omitempty"`
	SessionID    string    `json:"session_id,omitempty"` // Child session ID for inspector integration
	AgentID      string    `json:"agent_id,omitempty"`
	Status       string    `json:"status,omitempty"`
	Resumable    bool      `json:"resumable"`
	Next         string    `json:"next,omitempty"`
	TurnsUsed    int       `json:"turns_used,omitempty"`
	LastActivity time.Time `json:"last_activity,omitempty"`
	CurrentTool  string    `json:"current_tool,omitempty"`
	// Interventions records corrections a human made to this delegated run while
	// it was executing. They are local corrections inside the assignment this
	// call already made; they never redefine the assignment itself.
	Interventions []string `json:"interventions,omitempty"`
	// InterventionDisposition is queued, consumed, undelivered, or mixed.
	InterventionDisposition string `json:"intervention_disposition,omitempty"`
	// CancelledByUser separates a human stop from an execution failure.
	CancelledByUser bool `json:"cancelled_by_user,omitempty"`
}

// ParseSpawnAgentResult decodes the durable JSON contract returned by
// spawn_agent. Callers should use the matching tool call/result identity to
// decide whether content belongs to spawn_agent; this parser intentionally does
// not accept arbitrary plain text as a legacy result.
func ParseSpawnAgentResult(content string) (SpawnAgentResult, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return SpawnAgentResult{}, errors.New("empty spawn_agent result")
	}
	var result SpawnAgentResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return SpawnAgentResult{}, fmt.Errorf("decode spawn_agent result: %w", err)
	}
	if result.AgentName == "" && result.Output == "" && result.Error == "" && result.Type == "" && result.Duration == 0 && result.SessionID == "" && result.AgentID == "" && result.Status == "" {
		return SpawnAgentResult{}, errors.New("spawn_agent result has no recognized fields")
	}
	return result, nil
}

// SubagentEventType identifies the type of subagent event.
type SubagentEventType string

const (
	SubagentEventInit      SubagentEventType = "init" // Sent first with provider/model info
	SubagentEventText      SubagentEventType = "text"
	SubagentEventToolStart SubagentEventType = "tool_start"
	SubagentEventToolEnd   SubagentEventType = "tool_end"
	SubagentEventPhase     SubagentEventType = "phase"
	SubagentEventUsage     SubagentEventType = "usage"
	SubagentEventGuardian  SubagentEventType = "guardian"
	SubagentEventDone      SubagentEventType = "done"
)

// SubagentEvent represents an event from a running subagent.
type SubagentEvent struct {
	Type              SubagentEventType   // "init", "text", "tool_start", "tool_end", "phase", "usage", "done"
	Text              string              // for "text" events
	ToolName          string              // for tool events
	ToolCallID        string              // nested tool invocation ID
	ToolArgs          json.RawMessage     // nested tool arguments
	Guardian          *GuardianEvent      // for guardian events
	ToolInfo          string              // for tool events
	ToolOutput        string              // for "tool_end" events - text content
	Diffs             []llm.DiffData      // for "tool_end" events - structured diffs
	Images            []string            // for "tool_end" events - legacy image paths
	Media             []llm.MediaArtifact // for "tool_end" events - ordered image/video artifacts
	Success           bool                // for "tool_end" events
	Phase             string              // for "phase" events
	InputTokens       int                 // fresh input tokens for "usage" events
	OutputTokens      int                 // output tokens for "usage" events
	CountsTurn        bool                // usage from a completed child model turn, not compaction or nested usage
	CachedInputTokens int                 // cache-read tokens for "usage" events
	CacheWriteTokens  int                 // cache-write tokens for "usage" events
	Provider          string              // resolved provider for init/usage events
	Model             string              // resolved model for init/usage events
	Timestamp         time.Time           // authoritative child lifecycle/event time when available
	Deadline          time.Time           // independently enforced child/run deadline when available
	RunID             string              // queued run identity; never model-provided display text
	JobID             string              // queued job identity
	EventID           int64               // monotonic persisted queued-event identity; zero for synchronous events
	ProgressTruncated bool                // queued event history could not be read completely
}

// SubagentEventCallback is called to bubble up events from a running subagent.
// callID is the tool call ID of the spawn_agent call.
type SubagentEventCallback func(callID string, event SubagentEvent)

// SpawnAgentRunResult contains the output from a sub-agent run.
type SpawnAgentRunResult struct {
	Output    string // Text output from the agent
	SessionID string // Child session ID for inspector integration (empty if session tracking disabled)
	// Interventions and their aggregate disposition are reported by hosts that
	// can steer a live child. Hosts without that capability leave them empty.
	Interventions           []string
	InterventionDisposition string
	CancelledByUser         bool
}

// SpawnAgentCatalog is the optional read-only catalog exposed by runners that
// can prove which named agent definitions the current runtime can resolve.
// Capability checks never execute an agent or mutate tool permissions.
type SpawnAgentCatalog interface {
	ListSpawnAgentNames() ([]string, error)
	ResolveSpawnAgent(name string) error
}

// SpawnAgentRunner is the interface for running sub-agents.
// This is set by the cmd package to avoid circular imports.
type SpawnAgentRunner interface {
	// RunAgent runs a sub-agent and returns its text output.
	// ctx is used for cancellation, agentName is the agent to load,
	// prompt is the task, and depth is the current nesting level.
	RunAgent(ctx context.Context, agentName string, prompt string, depth int) (SpawnAgentRunResult, error)

	// RunAgentWithCallback runs a sub-agent with an event callback for progress reporting.
	// callID is used to correlate events with the parent's spawn_agent tool call.
	RunAgentWithCallback(ctx context.Context, agentName string, prompt string, depth int,
		callID string, cb SubagentEventCallback) (SpawnAgentRunResult, error)
}

type SpawnAgentRunOptions struct {
	ModelOverride  string
	ChildSessionID string
	BaseDir        string
	// RemainingDepth is the parent budget after spending one level. Nil means
	// no parent cap (for standalone runs). Zero is an exhausted child budget.
	RemainingDepth *int
}

// SpawnAgentRunnerWithOptions can run sub-agents with call-specific overrides.
type SpawnAgentRunnerWithOptions interface {
	RunAgentWithOptions(ctx context.Context, agentName string, prompt string, depth int, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error)
	RunAgentWithCallbackAndOptions(ctx context.Context, agentName string, prompt string, depth int,
		callID string, cb SubagentEventCallback, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error)
}

// DefaultSubagentMaxTurns is the turn budget for a delegated agent that does
// not set max_turns. Children are no longer killed by wall-clock timeouts, so
// the turn limit is their only budget; a child that exhausts it is resumable
// with continue_agent, which grants the same allotment again.
const DefaultSubagentMaxTurns = 500

// SpawnConfig configures spawn_agent behavior.
type SpawnConfig struct {
	MaxParallel    int               // Max concurrent sub-agents (default 3)
	MaxDepth       int               // Levels below this agent (default 2), capped by its parent budget
	DefaultTimeout int               // Default caller wait budget in seconds (default 300); never a child deadline
	AllowedAgents  []string          // Optional whitelist of allowed agents
	AgentModels    map[string]string // Optional per-spawn model overrides by agent name
}

// DefaultSpawnConfig returns the default spawn configuration.
func DefaultSpawnConfig() SpawnConfig {
	return SpawnConfig{
		MaxParallel:    3,
		MaxDepth:       2,
		DefaultTimeout: 300,
	}
}

// SpawnAgentTool implements the spawn_agent tool.
type SpawnAgentTool struct {
	runner         SpawnAgentRunner
	mediaPublisher MediaPublisher
	config         SpawnConfig
	semaphore      chan struct{}         // Limits concurrent agents
	depth          int                   // Absolute nesting depth for child run metadata
	remainingDepth int                   // Levels this agent may still spawn
	mu             sync.Mutex            // Protects runner updates
	eventCallback  SubagentEventCallback // Optional callback for event bubbling
	manager        *agentManager
}

// NewSpawnAgentTool creates a new spawn_agent tool.
func NewSpawnAgentTool(config SpawnConfig, depth int) *SpawnAgentTool {
	if config.MaxParallel <= 0 {
		config.MaxParallel = 3
	}
	if config.MaxDepth <= 0 {
		config.MaxDepth = 2
	}
	if config.DefaultTimeout <= 0 {
		config.DefaultTimeout = 300
	}

	return &SpawnAgentTool{
		config:         config,
		semaphore:      make(chan struct{}, config.MaxParallel),
		manager:        newAgentManager(config),
		depth:          depth,
		remainingDepth: config.MaxDepth,
	}
}

// SetRunner sets the runner for executing sub-agents.
// This must be called before Execute can succeed.
func (t *SpawnAgentTool) SetRunner(runner SpawnAgentRunner) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.runner = runner
	if setter, ok := runner.(interface{ SetAgentLifecycleTool(*SpawnAgentTool) }); ok {
		setter.SetAgentLifecycleTool(t)
	}
	t.manager.mu.Lock()
	t.manager.runner = runner
	t.manager.depth = t.depth
	if provider, ok := runner.(interface{ AgentRunStore() session.AgentRunStore }); ok {
		t.manager.store = provider.AgentRunStore()
	}
	t.manager.mu.Unlock()
	if t.mediaPublisher != nil {
		if setter, ok := t.runner.(interface{ SetMediaPublisher(MediaPublisher) }); ok {
			setter.SetMediaPublisher(t.mediaPublisher)
		}
	}
}

// SetMediaPublisher forwards serve-owned media publication to runners that
// support child-registry propagation without expanding the public runner API.
func (t *SpawnAgentTool) SetMediaPublisher(publisher MediaPublisher) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.mediaPublisher = publisher
	if setter, ok := t.runner.(interface{ SetMediaPublisher(MediaPublisher) }); ok {
		setter.SetMediaPublisher(publisher)
	}
}

// SetDepth sets the absolute nesting depth for child run metadata.
// Used when creating tools for sub-agents to track depth.
func (t *SpawnAgentTool) SetDepth(depth int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.depth = depth
	t.manager.mu.Lock()
	t.manager.depth = depth
	t.manager.mu.Unlock()
}

// SetRemainingDepth caps this agent's own spawn allowance by its parent's
// remaining budget. Unlike depth, zero is an explicit exhausted budget.
func (t *SpawnAgentTool) SetRemainingDepth(parentBudget int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if parentBudget < 0 {
		parentBudget = 0
	}
	if parentBudget < t.remainingDepth {
		t.remainingDepth = parentBudget
	}
	t.manager.mu.Lock()
	t.manager.remainingDepth = t.remainingDepth
	t.manager.mu.Unlock()
}

// RemainingDepth reports the current effective spawn allowance.
func (t *SpawnAgentTool) RemainingDepth() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.remainingDepth
}

// SetEventCallback sets the callback for receiving subagent progress events.
// Events are bubbled up to the parent for display during execution.
func (t *SpawnAgentTool) SetEventCallback(cb SubagentEventCallback) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.eventCallback = cb
}

// GetEventCallback returns the current event callback (thread-safe).
func (t *SpawnAgentTool) GetEventCallback() SubagentEventCallback {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.eventCallback
}

// Spec returns the tool specification.
func (t *SpawnAgentTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: SpawnAgentToolName,
		Description: `Spawn a sub-agent to handle a specific task autonomously. Use this to delegate work to specialized agents that can run in parallel.

Guidelines:
- Spawn multiple agents concurrently for independent analysis tasks
- Each agent runs with its own context and tools
- If the wait budget expires the child keeps running; use wait_agent, continue_agent, cancel_agent, or list_agents
- Use descriptive prompts that give the agent clear objectives
- Only set model when the user explicitly asks for a specific model/provider, and use exact provider:model format`,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_name": map[string]any{
					"type":        "string",
					"description": "Name of the agent to spawn (e.g., 'developer', 'codebase', 'web-researcher'). Use 'codebase' for local source code questions; 'web-researcher' for external web research only.",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task or prompt for the sub-agent to execute",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "Deprecated alias for wait; never cancels the child",
					"minimum":     0,
					"maximum":     3600,
				},
				"wait":             map[string]any{"type": "integer", "description": "Seconds to wait before detaching; 0 returns immediately (default 300)", "minimum": 0, "maximum": 3600},
				"notify_when_done": map[string]any{"type": "boolean", "description": "Wake the original parent agent loop when the child finishes; requires a supported persistent host session"},
				"cwd":              map[string]any{"type": "string", "description": "Optional existing child working directory; relative paths resolve against the parent's current working directory. Access still requires normal workspace approval."},
				"model": map[string]any{
					"type":        "string",
					"description": "Optional model override in exact provider:model format. If omitted, the sub-agent uses its configured/default model.",
					"pattern":     `^[^:\s]+:\S+$`,
				},
			},
			"required":             []string{"agent_name", "prompt"},
			"additionalProperties": false,
		},
	}
}

func isQualifiedSpawnModel(model string) bool {
	provider, modelName, found := strings.Cut(model, ":")
	return found && provider != "" && modelName != "" &&
		!strings.ContainsFunc(provider, unicode.IsSpace) &&
		!strings.ContainsFunc(modelName, unicode.IsSpace)
}

type spawnAgentPolicyError struct {
	typeName ToolErrorType
	message  string
}

func (e *spawnAgentPolicyError) Error() string { return e.message }

type localSpawnPolicySnapshot struct {
	runner         SpawnAgentRunner
	depth          int
	remainingDepth int
	allowedAgents  []string
}

func (t *SpawnAgentTool) snapshotLocalSpawnPolicy() localSpawnPolicySnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return localSpawnPolicySnapshot{
		runner:         t.runner,
		depth:          t.depth,
		remainingDepth: t.remainingDepth,
		allowedAgents:  append([]string(nil), t.config.AllowedAgents...),
	}
}

func (p localSpawnPolicySnapshot) authorize(agentName string, listing bool) error {
	if p.remainingDepth < 1 {
		return &spawnAgentPolicyError{
			typeName: ErrPermissionDenied,
			message:  "spawn depth budget exhausted (this agent may spawn 0 more levels)",
		}
	}
	if !listing {
		if agentName == "" {
			return &spawnAgentPolicyError{typeName: ErrInvalidParams, message: "agent name is required"}
		}
		if len(p.allowedAgents) > 0 {
			allowed := false
			for _, name := range p.allowedAgents {
				if name == agentName {
					allowed = true
					break
				}
			}
			if !allowed {
				return &spawnAgentPolicyError{
					typeName: ErrPermissionDenied,
					message:  fmt.Sprintf("agent '%s' is not in the allowed list", agentName),
				}
			}
		}
	}
	if p.runner == nil {
		return &spawnAgentPolicyError{typeName: ErrExecutionFailed, message: "spawn_agent runner not configured"}
	}
	return nil
}

// localSpawnPolicy applies the same depth, exact-whitelist, and runner checks
// used by Execute. State is copied while locked; callers may perform catalog I/O
// after this method returns without holding the tool mutex.
func (t *SpawnAgentTool) localSpawnPolicy(agentName string) (SpawnAgentRunner, int, error) {
	policy := t.snapshotLocalSpawnPolicy()
	if err := policy.authorize(agentName, false); err != nil {
		return nil, policy.depth, err
	}
	return policy.runner, policy.depth, nil
}

// CanSpawnAgent performs a read-only capability check against the current
// tool policy and the same runner catalog used by execution.
func (t *SpawnAgentTool) CanSpawnAgent(name string) error {
	if name == "" {
		return errors.New("agent name is required")
	}
	runner, _, err := t.localSpawnPolicy(name)
	if err != nil {
		return err
	}
	catalog, ok := runner.(SpawnAgentCatalog)
	if !ok {
		return errors.New("spawn_agent runner does not expose an agent catalog")
	}
	if err := catalog.ResolveSpawnAgent(name); err != nil {
		return fmt.Errorf("agent %q is unavailable or invalid: %w", name, err)
	}
	return nil
}

// PermittedAgentNames returns deterministic canonical lookup names that the
// current tool instance can spawn. Invalid definitions and names denied by the
// exact whitelist are omitted.
func (t *SpawnAgentTool) PermittedAgentNames() ([]string, error) {
	policy := t.snapshotLocalSpawnPolicy()
	if err := policy.authorize("", true); err != nil {
		return nil, err
	}
	catalog, ok := policy.runner.(SpawnAgentCatalog)
	if !ok {
		return nil, errors.New("spawn_agent runner does not expose an agent catalog")
	}
	names, err := catalog.ListSpawnAgentNames()
	if err != nil {
		return nil, fmt.Errorf("list spawnable agents: %w", err)
	}
	names = append([]string(nil), names...)
	sort.Strings(names)
	permitted := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		if err := policy.authorize(name, false); err != nil {
			continue
		}
		// ListSpawnAgentNames is the catalog's validated listing contract. Do not
		// resolve every entry a second time on each completion query.
		permitted = append(permitted, name)
	}
	return permitted, nil
}

func spawnAgentErrorOutput(content string, timedOut bool) llm.ToolOutput {
	output := llm.TextOutput(content)
	output.IsError = true
	output.TimedOut = timedOut
	return output
}

// Execute runs the spawn_agent tool.
func (t *SpawnAgentTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a SpawnAgentArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return spawnAgentErrorOutput(t.formatError(ErrInvalidParams, fmt.Sprintf("failed to parse arguments: %v", err)), false), nil
	}

	// Validate arguments
	if a.AgentName == "" {
		return spawnAgentErrorOutput(t.formatError(ErrInvalidParams, "agent_name is required"), false), nil
	}
	if a.Prompt == "" {
		return spawnAgentErrorOutput(t.formatError(ErrInvalidParams, "prompt is required"), false), nil
	}
	requestedModel := a.Model
	if requestedModel != "" && !isQualifiedSpawnModel(requestedModel) {
		return spawnAgentErrorOutput(t.formatError(ErrInvalidParams, "model must use exact provider:model format; omit it to use the configured/default model"), false), nil
	}

	runner, currentDepth, policyErr := t.localSpawnPolicy(a.AgentName)
	if policyErr != nil {
		errType := ErrExecutionFailed
		var typed *spawnAgentPolicyError
		if errors.As(policyErr, &typed) {
			errType = typed.typeName
		}
		return spawnAgentErrorOutput(t.formatError(errType, policyErr.Error()), false), nil
	}

	budget := t.config.DefaultTimeout
	if a.Timeout > 0 {
		budget = a.Timeout
	}
	if a.Wait != nil {
		budget = *a.Wait
	}
	if budget < 0 {
		return spawnAgentErrorOutput(t.formatError(ErrInvalidParams, "wait must be nonnegative"), false), nil
	}
	if budget > 3600 {
		budget = 3600
	}
	modelOverride := requestedModel
	if modelOverride == "" {
		modelOverride = strings.TrimSpace(t.config.AgentModels[a.AgentName])
	}
	baseDir, cwdErr := resolveAgentCWD(runner, a.CWD)
	if cwdErr != nil {
		return spawnAgentErrorOutput(t.formatError(ErrInvalidParams, cwdErr.Error()), false), nil
	}
	origin := t.trustedCompletionOrigin(ctx)
	if a.NotifyWhenDone && origin == "" {
		return spawnAgentErrorOutput(t.formatError(ErrInvalidParams, "notify_when_done requires a persistent web parent session with loop reactivation"), false), nil
	}
	callID := llm.CallIDFromContext(ctx)
	executionCallback := SubagentEventCallbackFromContext(ctx)
	cb := func(eventCallID string, event SubagentEvent) {
		emitExecutionSubagentEvent(executionCallback, callID, eventCallID, event)
	}
	entry, startErr := t.manager.start(ctx, a.AgentName, a.Prompt, modelOverride, callID, cb, t.GetEventCallback(), runner, currentDepth+1, false, "", session.AgentRun{BaseDir: baseDir, NotifyWhenDone: a.NotifyWhenDone, NotifyOrigin: origin})
	if startErr != nil {
		return spawnAgentErrorOutput(t.formatError(ErrExecutionFailed, startErr.Error()), false), nil
	}
	sessionless := entry.record.ParentSessionID == ""
	if sessionless {
		// A sessionless host cannot address a detached child with lifecycle tools.
		// Keep it synchronous with no execution deadline. Explicit parent stop
		// interrupts the child; wait is only a collection budget in sessions.
		select {
		case <-entry.done:
		case <-ctx.Done():
			t.manager.mu.Lock()
			entry.shutdown = true
			t.manager.mu.Unlock()
			entry.interrupt()
			<-entry.done
		}
	} else {
		t.manager.wait(ctx, entry, time.Duration(budget)*time.Second)
	}
	t.manager.detachInitial(entry)
	record, _, _ := t.manager.get(context.Background(), entry.record.ID, entry.record.ParentSessionID)
	out := t.manager.deliver(ctx, record, entry)
	if sessionless {
		out = withoutLifecycleHints(out)
	}
	return out, nil
}

// resolveAgentCWD snapshots the parent's actual directory before launch. An
// unbound web session cannot resolve relative paths against the daemon CWD.
func resolveAgentCWD(runner SpawnAgentRunner, cwd string) (string, error) {
	baseDir := ""
	if provider, ok := runner.(interface{ AgentWorkingDir() string }); ok {
		baseDir = provider.AgentWorkingDir()
	}
	if cwd == "" {
		return baseDir, nil
	}
	if !filepath.IsAbs(cwd) {
		if baseDir == "" {
			return "", errors.New("relative cwd requires a bound parent working directory")
		}
		cwd = filepath.Join(baseDir, cwd)
	}
	resolved, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("cwd must be an existing directory")
	}
	return resolved, nil
}

func (t *SpawnAgentTool) trustedCompletionOrigin(ctx context.Context) string {
	parent := llm.SessionIDFromContext(ctx)
	origin, ok := QueueAgentOriginFromContext(ctx)
	if parent != "" && ok && origin.Origin == QueueAgentOriginWeb && origin.SessionID == parent && agentCompletionWake(ctx) != nil && t.manager.store != nil {
		return QueueAgentOriginWeb
	}
	return ""
}

// withoutLifecycleHints drops resume/wait guidance that a session-less host
// cannot act on: every lifecycle tool requires a parent session.
func withoutLifecycleHints(out llm.ToolOutput) llm.ToolOutput {
	var result SpawnAgentResult
	if json.Unmarshal([]byte(out.Content), &result) != nil {
		return out
	}
	result.Resumable = false
	result.Next = ""
	out.Content = marshalAgentResult(result)
	return out
}

// OutstandingAgentIDs lists unfinished runs for one-shot host exit diagnostics.
func (t *SpawnAgentTool) OutstandingAgentIDs() []string { return t.manager.outstandingIDs() }

func (t *SpawnAgentTool) Drain(ctx context.Context) error { return t.manager.Drain(ctx) }

func (t *SpawnAgentTool) Shutdown(ctx context.Context) error { return t.manager.Shutdown(ctx) }

func (t *SpawnAgentTool) CancelDescendants(ctx context.Context) error {
	return t.manager.CancelDescendants(ctx)
}

// Preview returns a short description of the tool call.
func (t *SpawnAgentTool) Preview(args json.RawMessage) string {
	var a SpawnAgentArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ""
	}
	if a.AgentName == "" {
		return ""
	}
	// Truncate prompt for preview
	prompt := a.Prompt
	if len(prompt) > 50 {
		prompt = prompt[:47] + "..."
	}
	return fmt.Sprintf("@%s: %s", a.AgentName, prompt)
}

// classifySpawnAgentError maps runner/context errors to a stable tool error type.
func classifySpawnAgentError(err error, parentCtx, childCtx context.Context) ToolErrorType {
	if llm.IsMaxTurnsExceeded(err) {
		return ErrExecutionFailed
	}
	if errors.Is(err, context.DeadlineExceeded) || parentCtx.Err() == context.DeadlineExceeded || childCtx.Err() == context.DeadlineExceeded {
		return ErrTimeout
	}
	return ErrExecutionFailed
}

// formatError formats an error result.
func (t *SpawnAgentTool) formatError(errType ToolErrorType, message string) string {
	result := SpawnAgentResult{
		Error: message,
		Type:  string(errType),
	}
	data, _ := json.Marshal(result)
	return string(data)
}

// formatErrorWithDuration formats an error result with duration.
func (t *SpawnAgentTool) formatErrorWithDuration(errType ToolErrorType, message string, durationMs int64) string {
	return t.formatErrorWithPartialResult(errType, message, durationMs, SpawnAgentRunResult{})
}

// formatErrorWithPartialResult formats an error result while preserving partial subagent output/session metadata.
func (t *SpawnAgentTool) formatErrorWithPartialResult(errType ToolErrorType, message string, durationMs int64, runResult SpawnAgentRunResult) string {
	result := SpawnAgentResult{
		Output:                  runResult.Output,
		Error:                   message,
		Type:                    string(errType),
		Duration:                durationMs,
		SessionID:               runResult.SessionID,
		Interventions:           runResult.Interventions,
		InterventionDisposition: runResult.InterventionDisposition,
		CancelledByUser:         runResult.CancelledByUser,
	}
	data, _ := json.Marshal(result)
	return string(data)
}

func emitExecutionSubagentEvent(callback SubagentEventCallback, callID, eventCallID string, event SubagentEvent) {
	if callback != nil && callID != "" {
		callback(eventCallID, event)
	}
}
