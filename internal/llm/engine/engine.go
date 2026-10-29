package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

const (
	defaultMaxTurns                    = 50
	defaultMaxParallelToolCalls        = 20
	defaultUncommittedStreamMaxRetries = 5
	stopSearchToolHint                 = "IMPORTANT: Do not call any tools. Use the information already retrieved and answer directly."
	dynamicToolContinuationPrompt      = "Continue the task now that newly activated tools are available. Discover and use the new tools as required; do not repeat the activation call."
	PhaseCompacting                    = "Compacting"
	PhaseCompactingWriteBrief          = "Compacting: write brief"
	PhaseCompactingSummarizeHistory    = "Compacting: summarize history"
	PhaseCompactingResumeTask          = "Compacting: resume task"
	callbackTimeout                    = 5 * time.Second
	toolHeartbeatInterval              = 10 * time.Second
)

// getMaxTurns returns the max turns from request, with fallback to default
func getMaxTurns(req core.Request) int {
	if req.MaxTurns > 0 {
		return req.MaxTurns
	}
	return defaultMaxTurns
}

func maxParallelToolWorkers(callCount int) int {
	if callCount <= 0 {
		return 0
	}
	return min(callCount, defaultMaxParallelToolCalls)
}

// TurnCompletedCallback is called after each turn completes with the messages
// generated during that turn and metrics about the turn. turnIndex is 0-based.
// If ResponseCompletedCallback successfully handled the assistant message for
// this turn, messages contains only the later turn messages (usually tool
// results). Otherwise, messages contains the complete generated turn, including
// assistant message(s) and tool result(s). Recovery paths that bypass
// ResponseCompletedCallback also deliver the assistant message here.
type TurnCompletedCallback func(ctx context.Context, turnIndex int, messages []core.Message, metrics core.TurnMetrics) error

// ResponseCompletedCallback is called immediately after LLM streaming completes,
// BEFORE tool execution. This enables incremental persistence of assistant messages
// so they're saved even if the process crashes during tool execution.
// The message contains only the assistant's response (no tool results yet).
type ResponseCompletedCallback func(ctx context.Context, turnIndex int, assistantMsg core.Message, metrics core.TurnMetrics) error

// AssistantSnapshotCallback is called during streaming whenever accumulated
// assistant state materially changes (typically right before each EventToolCall
// emission, sync or async). Multiple fires per turn are expected; implementations
// MUST upsert the same logical row, not append. assistantMsg contains the
// in-progress message built from accumulated text/reasoning/toolCalls at the
// moment of firing. Used to persist "as we go" so content survives process
// death mid-turn (e.g., consumer cancels context between EventToolCall emission
// and tool execution).
type AssistantSnapshotCallback func(ctx context.Context, turnIndex int, assistantMsg core.Message) error

// RuntimeSwitch describes the exact request runtime transition applied between
// provider turns. Empty effort means the provider's automatic/default effort.
type RuntimeSwitch struct {
	PreviousModel           string
	PreviousReasoningEffort string
	Model                   string
	ReasoningEffort         string
	ProviderTurnIndex       int
	BoundaryID              string
}

// RuntimeSwitchCallback durably records an applied transition before the next
// provider turn can produce output.
type RuntimeSwitchCallback func(ctx context.Context, change RuntimeSwitch) error

// CompactionCallback is called after context compaction to allow callers to
// update their state (e.g., replace in-memory messages, persist changes). The
// callback must synchronously replace/persist the owner's active context before
// returning; the engine only updates its in-flight request copy, so owner state
// that is not updated here can resurrect pre-compaction history later.
type CompactionCallback func(ctx context.Context, result *CompactionResult) error

// Engine orchestrates provider calls and external tool execution.
type pendingRequestRuntimeSwitch struct {
	model           string
	reasoningEffort string
}

// FileTrackingRunLifecycle persists run boundaries independently of file changes.
type FileTrackingRunLifecycle interface {
	RecordFileTrackingRunStart(context.Context, string, string) error
	RecordFileTrackingRunComplete(context.Context, string, string) error
}

type Engine struct {
	provider         core.Provider
	tools            *ToolRegistry
	debugLogger      DebugLogger
	fileTrackingRuns FileTrackingRunLifecycle

	// indirectVision routes user image parts through textual path references so
	// text-only models can call view_image instead of receiving image bytes.
	indirectVision atomic.Bool

	// allowedTools filters which tools can be executed. A nil map means no
	// filter; a non-nil empty map is an explicit filter allowing no tools.
	// Used by skills with a present allowed-tools field.
	allowedTools map[string]bool
	allowedMu    sync.RWMutex

	// onTurnCompleted is called after each turn with messages generated.
	// Used for incremental session saving. Protected by callbackMu.
	onTurnCompleted TurnCompletedCallback
	// onResponseCompleted is called immediately after LLM streaming completes,
	// BEFORE tool execution. Used for incremental persistence of assistant messages.
	onResponseCompleted ResponseCompletedCallback
	// onAssistantSnapshot is called during streaming whenever accumulated
	// assistant state materially changes (typically right before each EventToolCall
	// emission). Implementations MUST upsert the same logical row.
	onAssistantSnapshot AssistantSnapshotCallback
	// onRuntimeSwitch is called after the preceding turn is committed and before
	// the newly selected runtime can produce output.
	onRuntimeSwitch RuntimeSwitchCallback
	// onCompaction is called after context compaction completes.
	onCompaction           CompactionCallback
	callbackMu             sync.RWMutex
	steeringTransition     *SteeringTransition
	steeringTransitionDone chan struct{}
	activeSteeringTools    int
	steeringToolsSettled   chan struct{}

	// Global tool output truncation
	maxToolOutputChars int // 0 = disabled; truncate tool output to this many runes

	// Context compaction
	compactionConfig     *CompactionConfig // nil = compaction disabled
	inputLimit           int               // 0 = unknown/disabled
	lastTotalTokens      int               // cached+input+output from most recent API response
	lastMessageCount     int               // retained/persisted for compatibility; estimator anchors structurally
	reportedContext      core.ContextUsage // display-only reading from providers that manage their own context
	reportedContextKey   string            // provider/model the reportedContext belongs to
	systemPrompt         string            // Captured for re-injection after compaction
	contextNoticeEmitted atomic.Bool       // one-shot flag: WARNING emitted once per session

	// Steering support: users can send messages while the agent is streaming.
	// Messages are injected FIFO after the current turn's tool results, before
	// the next LLM turn. While entries remain in this queue they are cancellable;
	// draining atomically commits them for the next provider request.
	pendingSteering        []queuedSteering
	claimedSteeringIDs     map[string]struct{}
	claimedSteeringOrder   []string
	committedSteeringIDs   map[string]struct{}
	committedSteeringOrder []string
	steeringRunState       steeringRunState

	// pendingRequestRuntime is a same-provider model/effort override requested
	// while an agentic loop is active. It is drained at the next provider-turn
	// boundary so UI effort changes can affect the next LLM call after tool
	// results without replacing the in-flight Engine.
	pendingRequestRuntime pendingRequestRuntimeSwitch

	// Protected by callbackMu; consumed immediately before the next provider request.
	pendingServiceTier *string

	// chaosFailNext is armed by TERM_LLM_CHAOS_MONKEY UI shortcuts to inject a
	// replayable stream failure at the next provider receive boundary.
	chaosFailNext atomic.Bool

	// pendingToolSpecs is a run-scoped delivery queue for tools activated between
	// provider turns. It is never authoritative session state.
	pendingToolSpecs map[string]map[string]core.ToolSpec
	activeToolRunID  string
	pendingToolsMu   sync.Mutex
	toolRunPrefix    string
	toolRunCounter   atomic.Uint64

	toolPlannerMu sync.RWMutex
	toolPlanner   core.ToolSurfacePlanner
}

// ToolExecutorSetter is an optional interface for providers that need
// tool execution wired up externally (for example, local CLI providers with HTTP MCP).
type ToolExecutorSetter interface {
	SetToolExecutor(func(ctx context.Context, name string, args json.RawMessage) (core.ToolOutput, error))
}

// DynamicToolPublisher is implemented by providers that must react while a
// tool loop is active inside one Stream call. Implementations may publish the
// schema live or arrange a controlled provider boundary before the next tool.
type DynamicToolPublisher interface {
	PublishDynamicTools([]core.ToolSpec) error
}

// InlineFlusher is implemented by inline-loop providers that can end the
// current CLI prompt at the next tool-result boundary. The engine requests a
// flush when an steering is queued so the following Stream can deliver it.
// RetryProvider implements the methods so factory wrapping stays transparent;
// SupportsInlineFlush reports whether the inner provider can actually flush.
type InlineFlusher interface {
	RequestInlineFlush()
	SupportsInlineFlush() bool
}

// ImmediateInterrupter reports that a provider can end an in-flight model turn
// without waiting for a tool boundary. Engines use the normal agentic loop for
// these providers even when no tools are exposed so queued steering have a
// continuation boundary to consume.
type ImmediateInterrupter interface {
	SupportsImmediateInterruption() bool
}

// InlineFlushResetter clears a flush request at an engine-controlled provider
// turn boundary. Keeping this outside provider Stream implementations preserves
// a request while RetryProvider re-enters the same logical turn.
type InlineFlushResetter interface {
	ClearInlineFlush()
}

// ProviderCleaner is an optional interface for providers that need cleanup
// after a conversation ends (for example, a local CLI provider's persistent MCP server).
// Call sites: runtime eviction, server shutdown. Do NOT call per-turn.
type ProviderCleaner interface {
	CleanupMCP()
}

// ProviderTurnCleaner is an optional interface for providers that need cleanup
// after each turn's stream ends (e.g., temp image files materialised for a
// single turn). Engine wraps agentic streams to invoke this on stream
// termination as a safety net for consumers that drop streams without Close().
type ProviderTurnCleaner interface {
	CleanupTurn()
}

type SteeringQueueStatus string

const (
	SteeringQueueTransitioning SteeringQueueStatus = "transitioning"
	SteeringQueueRushOwned     SteeringQueueStatus = "rush_owned"
	SteeringQueueQueued        SteeringQueueStatus = "queued"
	SteeringQueueAlreadyQueued SteeringQueueStatus = "already_queued"
	SteeringQueueFollowUpOwned SteeringQueueStatus = "follow_up_owned"
	SteeringQueueCommitted     SteeringQueueStatus = "committed"
	SteeringQueueRunFinished   SteeringQueueStatus = "run_finished"
)

type steeringRunState uint8

const (
	steeringRunIdle steeringRunState = iota
	steeringRunAccepting
	steeringRunNonConsuming
)

type SteeringClaimStatus string

const (
	SteeringClaimRushOwned     SteeringClaimStatus = "rush_owned"
	SteeringClaimNotFound      SteeringClaimStatus = "not_found"
	SteeringClaimed            SteeringClaimStatus = "claimed"
	SteeringClaimFollowUpOwned SteeringClaimStatus = "follow_up_owned"
	SteeringClaimCommitted     SteeringClaimStatus = "committed"
)

// QueuedSteering is a structured user message submitted while a run is active.
// Queued entries are cancellable until the engine drains them into a provider turn.
type SteeringOrigin string

const (
	SteeringOriginUser            SteeringOrigin = "user"
	SteeringOriginReview          SteeringOrigin = "review"
	SteeringOriginJobNotification SteeringOrigin = "job_notification"
	SteeringOriginLegacy          SteeringOrigin = "legacy_unknown"
)

// EligibleForRush excludes notification-only queues, without changing message roles.
func (o SteeringOrigin) EligibleForRush() bool { return o != SteeringOriginJobNotification }

type QueuedSteering struct {
	ID          string
	Origin      SteeringOrigin
	Message     core.Message
	DisplayText string
	Status      core.SteeringStatus
}

type queuedSteering = QueuedSteering

var engineSteeringID atomic.Uint64

const steeringIdentityLimit = 1024

func nextEngineSteeringID() string {
	return fmt.Sprintf("steer_%d", engineSteeringID.Add(1))
}

var engineIdentityFallback atomic.Uint64

func newEngineToolRunPrefix() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}
	return fmt.Sprintf("%x_%x", time.Now().UnixNano(), engineIdentityFallback.Add(1))
}

func NewEngine(provider core.Provider, tools *ToolRegistry) *Engine {
	if tools == nil {
		tools = NewToolRegistry()
	}
	e := &Engine{
		provider:         provider,
		tools:            tools,
		pendingToolSpecs: make(map[string]map[string]core.ToolSpec),
		toolRunPrefix:    newEngineToolRunPrefix(),
	}

	// Wire up tool executors for providers that expose term-llm tools over an external bridge.
	if setter, ok := provider.(ToolExecutorSetter); ok {
		setter.SetToolExecutor(func(ctx context.Context, name string, args json.RawMessage) (core.ToolOutput, error) {
			ctx, release, err := restart.Child(ctx)
			if err != nil {
				return core.ToolOutput{}, err
			}
			defer release()
			tool, ok := e.tools.Get(name)
			if !ok {
				return core.ToolOutput{}, fmt.Errorf("tool not found: %s", name)
			}
			// Native provider bridges share the same actual-completion barrier as
			// engine-managed calls, including after their process is cancelled.
			if err := e.beginSteeringTool(ctx); err != nil {
				return core.ToolOutput{}, err
			}
			defer e.actualSteeringToolDone()
			return tool.Execute(ctx, args)
		})
	}

	return e
}

// TriggerChaosFailure arms a one-shot synthetic replayable stream failure. It is
// intentionally tiny and transport-shaped so UI/debug flows exercise the same
// recovery paths as a prematurely closed SSE/WebSocket stream.
func (e *Engine) TriggerChaosFailure() {
	if e == nil {
		return
	}
	e.chaosFailNext.Store(true)
}

func (e *Engine) consumeChaosFailure() error {
	if e == nil || !e.chaosFailNext.Swap(false) {
		return nil
	}
	return &corestream.StreamIncompleteError{Transport: "simulated stream", Terminal: "completion"}
}

// SetIndirectVision enables or disables image reference mode. When enabled,
// user image parts are not sent to the primary provider. Instead, provider
// requests contain textual file-path references and an instruction to call the
// view_image tool when visual content matters.
func (e *Engine) SetIndirectVision(enabled bool) {
	if e == nil {
		return
	}
	e.indirectVision.Store(enabled)
}

// IndirectVision reports whether image reference mode is enabled.
func (e *Engine) IndirectVision() bool {
	if e == nil {
		return false
	}
	return e.indirectVision.Load()
}

// RegisterTool adds a tool to the engine's registry.
func (e *Engine) RegisterTool(tool core.Tool) {
	e.tools.Register(tool)
}

// SetToolSurfacePlanner installs the planner that owns dynamic provider visibility.
func (e *Engine) SetToolSurfacePlanner(planner core.ToolSurfacePlanner) {
	e.toolPlannerMu.Lock()
	e.toolPlanner = planner
	e.toolPlannerMu.Unlock()
}

// ClearToolSurfacePlanner removes planner only when it still owns this engine.
// This prevents an old planner from detaching a newer replacement.
func (e *Engine) ClearToolSurfacePlanner(planner core.ToolSurfacePlanner) bool {
	if e == nil || planner == nil {
		return false
	}
	e.toolPlannerMu.Lock()
	defer e.toolPlannerMu.Unlock()
	if e.toolPlanner != planner {
		return false
	}
	e.toolPlanner = nil
	return true
}

func (e *Engine) currentToolPlanner() core.ToolSurfacePlanner {
	e.toolPlannerMu.RLock()
	defer e.toolPlannerMu.RUnlock()
	return e.toolPlanner
}

// ToolDiscoveryDiagnostics returns current planner diagnostics when available.
func (e *Engine) ToolDiscoveryDiagnostics(sessionID string) (core.ToolDiscoveryDiagnostics, bool) {
	planner := e.currentToolPlanner()
	diagnoser, ok := planner.(core.ToolDiscoveryDiagnoser)
	if !ok {
		return core.ToolDiscoveryDiagnostics{}, false
	}
	return diagnoser.Diagnostics(sessionID), true
}

// ToolDiscoveryActiveSpecs returns the currently active MCP schemas for a
// session. It excludes catalogue entries that remain deferred.
func (e *Engine) ToolDiscoveryActiveSpecs(sessionID string) []core.ToolSpec {
	planner := e.currentToolPlanner()
	inspector, ok := planner.(core.ToolDiscoverySurfaceInspector)
	if !ok {
		return nil
	}
	return inspector.ActiveToolSpecs(sessionID)
}

// AddDynamicTool registers a tool and queues it for the currently active run.
// Calls outside a run only register the tool; durable activation belongs to the planner/tool.
func (e *Engine) AddDynamicTool(tool core.Tool) {
	e.tools.Register(tool)
	e.pendingToolsMu.Lock()
	runID := e.activeToolRunID
	e.pendingToolsMu.Unlock()
	if runID != "" {
		e.AddDynamicToolForRun(runID, tool)
	}
}

// AddDynamicToolForRun queues a provider schema only for the matching active run.
func (e *Engine) AddDynamicToolForRun(runID string, tool core.Tool) bool {
	if tool == nil || runID == "" {
		return false
	}
	spec := tool.Spec()
	e.pendingToolsMu.Lock()
	if e.activeToolRunID != runID {
		e.pendingToolsMu.Unlock()
		return false
	}
	pending := e.pendingToolSpecs[runID]
	if pending == nil {
		pending = make(map[string]core.ToolSpec)
		e.pendingToolSpecs[runID] = pending
	}
	pending[spec.Name] = spec
	e.pendingToolsMu.Unlock()

	if publisher, ok := e.provider.(DynamicToolPublisher); ok {
		if err := publisher.PublishDynamicTools([]core.ToolSpec{spec}); err != nil {
			// Keep the normal pending queue as a fallback for the next provider
			// turn, but make the inline publication failure diagnosable.
			runtimeoutput.Warn("publish dynamic tool to active provider", "tool", spec.Name, "run_id", runID, "error", err)
		}
	}
	return true
}

func (e *Engine) fileTrackingRunRecorder() FileTrackingRunLifecycle {
	e.callbackMu.RLock()
	defer e.callbackMu.RUnlock()
	return e.fileTrackingRuns
}

func (e *Engine) recordFileTrackingRunStart(ctx context.Context, sessionID, runID string) {
	recorder := e.fileTrackingRunRecorder()
	if recorder == nil || sessionID == "" || runID == "" {
		return
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := recorder.RecordFileTrackingRunStart(recordCtx, sessionID, runID); err != nil {
		runtimeoutput.Warn("record file tracking run start", "session_id", sessionID, "run_id", runID, "error", err)
	}
}

func (e *Engine) recordFileTrackingRunComplete(ctx context.Context, sessionID, runID string) {
	recorder := e.fileTrackingRunRecorder()
	if recorder == nil || sessionID == "" || runID == "" {
		return
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := recorder.RecordFileTrackingRunComplete(recordCtx, sessionID, runID); err != nil {
		runtimeoutput.Warn("record file tracking run completion", "session_id", sessionID, "run_id", runID, "error", err)
	}
}

func (e *Engine) beginToolRun() string {
	if e.toolRunPrefix == "" {
		e.toolRunPrefix = newEngineToolRunPrefix()
	}
	runID := fmt.Sprintf("toolrun_%s_%d", e.toolRunPrefix, e.toolRunCounter.Add(1))
	e.pendingToolsMu.Lock()
	// An engine runs one provider stream at a time. Clear abandoned delivery
	// queues so cancellation cannot leak activation into a later run.
	e.pendingToolSpecs = make(map[string]map[string]core.ToolSpec)
	e.activeToolRunID = runID
	e.pendingToolsMu.Unlock()
	return runID
}

func (e *Engine) endToolRun(runID string) {
	e.pendingToolsMu.Lock()
	delete(e.pendingToolSpecs, runID)
	if e.activeToolRunID == runID {
		e.activeToolRunID = ""
	}
	e.pendingToolsMu.Unlock()
}

func (e *Engine) hasPendingToolSpecs(runID string) bool {
	e.pendingToolsMu.Lock()
	defer e.pendingToolsMu.Unlock()
	return len(e.pendingToolSpecs[runID]) > 0
}

func (e *Engine) requestInlineFlush() {
	if flusher, ok := e.provider.(InlineFlusher); ok {
		flusher.RequestInlineFlush()
	}
}

func (e *Engine) providerSupportsInlineFlush() bool {
	flusher, ok := e.provider.(InlineFlusher)
	return ok && flusher.SupportsInlineFlush()
}

func (e *Engine) providerSupportsImmediateInterruption() bool {
	interrupter, ok := e.provider.(ImmediateInterrupter)
	return ok && interrupter.SupportsImmediateInterruption()
}

func (e *Engine) ClearInlineFlush() {
	if resetter, ok := e.provider.(InlineFlushResetter); ok {
		resetter.ClearInlineFlush()
	}
}

// drainPendingToolSpecs returns queued specs for one run and clears that queue.
func (e *Engine) drainPendingToolSpecs(runID string) []core.ToolSpec {
	e.pendingToolsMu.Lock()
	defer e.pendingToolsMu.Unlock()
	pending := e.pendingToolSpecs[runID]
	delete(e.pendingToolSpecs, runID)
	if len(pending) == 0 {
		return nil
	}
	names := make([]string, 0, len(pending))
	for name := range pending {
		names = append(names, name)
	}
	sort.Strings(names)
	specs := make([]core.ToolSpec, 0, len(names))
	for _, name := range names {
		specs = append(specs, pending[name])
	}
	return specs
}

// UnregisterTool removes a tool from the engine's registry.
func (e *Engine) UnregisterTool(name string) {
	e.tools.Unregister(name)
}

// Tools returns the engine's tool registry.
func (e *Engine) Tools() *ToolRegistry {
	return e.tools
}

func resetProviderConversation(provider core.Provider) {
	type conversationResetter interface {
		ResetConversation()
	}
	if r, ok := provider.(conversationResetter); ok {
		r.ResetConversation()
	}
}

// ResetConversation clears all conversation-specific state from the engine.
// Called on /clear or /new to start a fresh conversation. This resets
// compaction tracking, context notices, and provider-side conversation state
// (e.g., OpenAI Responses API previous_response_id).
func (e *Engine) ResetConversation() {
	e.callbackMu.Lock()
	e.lastTotalTokens = 0
	e.lastMessageCount = 0
	e.reportedContext = core.ContextUsage{}
	e.systemPrompt = ""
	e.claimedSteeringIDs = nil
	e.claimedSteeringOrder = nil
	e.committedSteeringIDs = nil
	e.committedSteeringOrder = nil
	e.steeringRunState = steeringRunIdle
	e.pendingServiceTier = nil
	e.contextNoticeEmitted.Store(false)
	e.callbackMu.Unlock()

	e.pendingToolsMu.Lock()
	e.pendingToolSpecs = make(map[string]map[string]core.ToolSpec)
	e.activeToolRunID = ""
	e.pendingToolsMu.Unlock()

	// Reset provider-side conversation state if supported
	resetProviderConversation(e.provider)
}

// ResetSessionState resets provider continuation and any tool-owned cached
// projection for a durable session. The durable store remains authoritative.
func (e *Engine) ResetSessionState(sessionID string) {
	if e == nil {
		return
	}
	e.ResetConversation()
	if planner := e.currentToolPlanner(); planner != nil {
		planner.ResetSession(sessionID)
	}
	for _, spec := range e.tools.AllSpecsIncludingDeferred() {
		tool, ok := e.tools.Get(spec.Name)
		if !ok {
			continue
		}
		if resetter, ok := tool.(SessionStateResetter); ok {
			resetter.ResetSessionState(sessionID)
		}
	}
}

// SetDebugLogger sets the debug logger for this engine.
func (e *Engine) SetDebugLogger(logger DebugLogger) {
	if isNilDebugLogger(logger) {
		logger = nil
	}
	e.debugLogger = logger
}

// SetFileTrackingRunLifecycle installs best-effort persisted run indexing.
func (e *Engine) SetFileTrackingRunLifecycle(recorder FileTrackingRunLifecycle) {
	e.callbackMu.Lock()
	e.fileTrackingRuns = recorder
	e.callbackMu.Unlock()
}

// SetAllowedTools sets the list of tools that can be executed.
// When set, only tools in this list can run; all others are blocked.
// Pass nil or empty slice to allow all tools.
// Late-bound names are retained, but execution still requires registration.
func (e *Engine) SetAllowedTools(tools []string) {
	e.allowedMu.Lock()
	defer e.allowedMu.Unlock()

	if len(tools) == 0 {
		e.allowedTools = nil
		return
	}

	e.allowedTools = make(map[string]bool, len(tools))
	for _, name := range tools {
		// Preserve explicit names for tools registered later (notably MCP catalogue
		// entries and dynamically activated skills). Execution still requires a
		// registered wrapper, so this grants no authority to a nonexistent tool.
		e.allowedTools[name] = true
	}
}

// SetAllowedToolsFilter applies a present tool allowlist. Unlike
// SetAllowedTools, an empty slice is meaningful and blocks every callable tool.
func (e *Engine) SetAllowedToolsFilter(tools []string) {
	e.allowedMu.Lock()
	defer e.allowedMu.Unlock()
	e.allowedTools = e.intersectAllowedTools(tools)
}

func (e *Engine) intersectAllowedTools(tools []string) map[string]bool {
	allowed := make(map[string]bool, len(tools))
	for _, name := range tools {
		// Keep late-bound names; registry lookup remains mandatory at execution.
		allowed[name] = true
	}
	return allowed
}

// AllowedToolsFilter returns a copy of the active filter and whether a filter
// is present. It is used to restore a temporary per-turn skill restriction.
func (e *Engine) AllowedToolsFilter() (tools []string, present bool) {
	e.allowedMu.RLock()
	defer e.allowedMu.RUnlock()
	if e.allowedTools == nil {
		return nil, false
	}
	tools = make([]string, 0, len(e.allowedTools))
	for name := range e.allowedTools {
		tools = append(tools, name)
	}
	sort.Strings(tools)
	return tools, true
}

// FilterAllowedToolSpecs removes tool definitions that cannot execute under the
// active allowlist. An omitted filter returns specs unchanged; a present empty
// filter returns an empty non-nil slice.
func (e *Engine) FilterAllowedToolSpecs(specs []core.ToolSpec) []core.ToolSpec {
	e.allowedMu.RLock()
	if e.allowedTools == nil {
		e.allowedMu.RUnlock()
		return specs
	}
	allowed := make(map[string]bool, len(e.allowedTools))
	for name, value := range e.allowedTools {
		allowed[name] = value
	}
	e.allowedMu.RUnlock()
	filtered := make([]core.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if allowed[spec.Name] {
			filtered = append(filtered, spec)
		}
	}
	return filtered
}

// RestoreAllowedToolsFilter restores a filter captured by AllowedToolsFilter.
func (e *Engine) RestoreAllowedToolsFilter(tools []string, present bool) {
	if !present {
		e.ClearAllowedTools()
		return
	}
	e.SetAllowedToolsFilter(tools)
}

// ClearAllowedTools removes the tool filter, allowing all registered tools.
func (e *Engine) ClearAllowedTools() {
	e.allowedMu.Lock()
	defer e.allowedMu.Unlock()
	e.allowedTools = nil
}

// SetTurnCompletedCallback sets the callback for incremental turn completion.
// The callback receives messages generated each turn for incremental persistence.
// Thread-safe: can be called while streaming is in progress.
func (e *Engine) SetTurnCompletedCallback(cb TurnCompletedCallback) {
	e.callbackMu.Lock()
	e.onTurnCompleted = cb
	e.callbackMu.Unlock()
}

// SetResponseCompletedCallback sets the callback for response completion (before tool execution).
// The callback receives the assistant message immediately after streaming completes.
// Thread-safe: can be called while streaming is in progress.
func (e *Engine) SetResponseCompletedCallback(cb ResponseCompletedCallback) {
	e.callbackMu.Lock()
	e.onResponseCompleted = cb
	e.callbackMu.Unlock()
}

// SetAssistantSnapshotCallback sets the callback fired during streaming whenever
// accumulated assistant state materially changes. Implementations MUST upsert
// the same logical row (keyed by turn index), not append. Used to persist "as we
// go" so content survives process death mid-turn.
// Thread-safe: can be called while streaming is in progress.
func (e *Engine) SetAssistantSnapshotCallback(cb AssistantSnapshotCallback) {
	e.callbackMu.Lock()
	e.onAssistantSnapshot = cb
	e.callbackMu.Unlock()
}

// SetRuntimeSwitchCallback sets the callback that records an applied request
// runtime transition before the target provider turn begins.
func (e *Engine) SetRuntimeSwitchCallback(cb RuntimeSwitchCallback) {
	e.callbackMu.Lock()
	e.onRuntimeSwitch = cb
	e.callbackMu.Unlock()
}

// getTurnCallback returns the current turn callback under read lock.
func (e *Engine) getTurnCallback() TurnCompletedCallback {
	e.callbackMu.RLock()
	cb := e.onTurnCompleted
	e.callbackMu.RUnlock()
	return cb
}

// getResponseCallback returns the current response callback under read lock.
func (e *Engine) getResponseCallback() ResponseCompletedCallback {
	e.callbackMu.RLock()
	cb := e.onResponseCompleted
	e.callbackMu.RUnlock()
	return cb
}

// getSnapshotCallback returns the current assistant-snapshot callback under read lock.
func (e *Engine) getSnapshotCallback() AssistantSnapshotCallback {
	e.callbackMu.RLock()
	cb := e.onAssistantSnapshot
	e.callbackMu.RUnlock()
	return cb
}

func (e *Engine) getRuntimeSwitchCallback() RuntimeSwitchCallback {
	e.callbackMu.RLock()
	cb := e.onRuntimeSwitch
	e.callbackMu.RUnlock()
	return cb
}

// callbackContext returns a context for persistence callbacks that should
// survive stream cancellation long enough to commit data.
func callbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), callbackTimeout)
}

func callResponseCompletedCallback(ctx context.Context, cb ResponseCompletedCallback, turnIndex int, assistantMsg core.Message, metrics core.TurnMetrics) bool {
	if cb == nil {
		return false
	}
	cbCtx, cancel := callbackContext(ctx)
	err := cb(cbCtx, turnIndex, assistantMsg, metrics)
	cancel()
	return err == nil
}

// turnMessagesAfterResponseCallback returns the messages that still need to be
// delivered to TurnCompletedCallback. When ResponseCompletedCallback has
// successfully handled the assistant message, TurnCompletedCallback emits only
// the subsequent messages for that turn (typically tool results). If the
// response callback was absent or failed, the assistant remains in the turn
// callback so callers still have a complete durable turn to persist.
func turnMessagesAfterResponseCallback(responseHandled bool, assistantMsg core.Message, rest []core.Message) []core.Message {
	if responseHandled {
		return rest
	}
	messages := make([]core.Message, 0, 1+len(rest))
	messages = append(messages, assistantMsg)
	messages = append(messages, rest...)
	return messages
}
