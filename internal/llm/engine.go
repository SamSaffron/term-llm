package llm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/appdata"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/usage"
)

const (
	defaultMaxTurns                    = 50
	defaultMaxParallelToolCalls        = 20
	defaultUncommittedStreamMaxRetries = 5
	stopSearchToolHint                 = "IMPORTANT: Do not call any tools. Use the information already retrieved and answer directly."
	contextContinuationPrompt          = "Continue the task from the compacted context. Follow the pending next step; do not ask the user unless blocked."
	dynamicToolContinuationPrompt      = "Continue the task now that newly activated tools are available. Discover and use the new tools as required; do not repeat the activation call."
	PhaseCompacting                    = "Compacting"
	PhaseCompactingWriteBrief          = "Compacting: write brief"
	PhaseCompactingSummarizeHistory    = "Compacting: summarize history"
	PhaseCompactingResumeTask          = "Compacting: resume task"
	callbackTimeout                    = 5 * time.Second
	toolHeartbeatInterval              = 10 * time.Second
)

// getMaxTurns returns the max turns from request, with fallback to default
func getMaxTurns(req Request) int {
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

// TurnMetrics contains metrics collected during a turn.
type TurnMetrics struct {
	InputTokens       int // Non-cached, non-cache-write input tokens this turn
	OutputTokens      int // Tokens generated as output this turn
	CachedInputTokens int // Input tokens served from cache (cache read) this turn
	CacheWriteTokens  int // Input tokens written to cache (cache creation) this turn
	ToolCalls         int // Number of tools executed this turn
}

// TurnCompletedCallback is called after each turn completes with the messages
// generated during that turn and metrics about the turn. turnIndex is 0-based.
// If ResponseCompletedCallback successfully handled the assistant message for
// this turn, messages contains only the later turn messages (usually tool
// results). Otherwise, messages contains the complete generated turn, including
// assistant message(s) and tool result(s). Recovery paths that bypass
// ResponseCompletedCallback also deliver the assistant message here.
type TurnCompletedCallback func(ctx context.Context, turnIndex int, messages []Message, metrics TurnMetrics) error

// ResponseCompletedCallback is called immediately after LLM streaming completes,
// BEFORE tool execution. This enables incremental persistence of assistant messages
// so they're saved even if the process crashes during tool execution.
// The message contains only the assistant's response (no tool results yet).
type ResponseCompletedCallback func(ctx context.Context, turnIndex int, assistantMsg Message, metrics TurnMetrics) error

// AssistantSnapshotCallback is called during streaming whenever accumulated
// assistant state materially changes (typically right before each EventToolCall
// emission, sync or async). Multiple fires per turn are expected; implementations
// MUST upsert the same logical row, not append. assistantMsg contains the
// in-progress message built from accumulated text/reasoning/toolCalls at the
// moment of firing. Used to persist "as we go" so content survives process
// death mid-turn (e.g., consumer cancels context between EventToolCall emission
// and tool execution).
type AssistantSnapshotCallback func(ctx context.Context, turnIndex int, assistantMsg Message) error

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
	provider         Provider
	tools            *ToolRegistry
	debugLogger      *DebugLogger
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
	pendingToolSpecs map[string]map[string]ToolSpec
	activeToolRunID  string
	pendingToolsMu   sync.Mutex
	toolRunPrefix    string
	toolRunCounter   atomic.Uint64

	toolPlannerMu sync.RWMutex
	toolPlanner   ToolSurfacePlanner
}

// ToolExecutorSetter is an optional interface for providers that need
// tool execution wired up externally (for example, local CLI providers with HTTP MCP).
type ToolExecutorSetter interface {
	SetToolExecutor(func(ctx context.Context, name string, args json.RawMessage) (ToolOutput, error))
}

// DynamicToolPublisher is implemented by providers that must react while a
// tool loop is active inside one Stream call. Implementations may publish the
// schema live or arrange a controlled provider boundary before the next tool.
type DynamicToolPublisher interface {
	PublishDynamicTools([]ToolSpec) error
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

// inlineFlushResetter clears a flush request at an engine-controlled provider
// turn boundary. Keeping this outside provider Stream implementations preserves
// a request while RetryProvider re-enters the same logical turn.
type inlineFlushResetter interface {
	clearInlineFlush()
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

type SteeringStatus string

const (
	SteeringQueued    SteeringStatus = "queued"
	SteeringCommitted SteeringStatus = "committed"
)

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
	Message     Message
	DisplayText string
	Status      SteeringStatus
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

func NewEngine(provider Provider, tools *ToolRegistry) *Engine {
	if tools == nil {
		tools = NewToolRegistry()
	}
	e := &Engine{
		provider:         provider,
		tools:            tools,
		pendingToolSpecs: make(map[string]map[string]ToolSpec),
		toolRunPrefix:    newEngineToolRunPrefix(),
	}

	// Wire up tool executors for providers that expose term-llm tools over an external bridge.
	if setter, ok := provider.(ToolExecutorSetter); ok {
		setter.SetToolExecutor(func(ctx context.Context, name string, args json.RawMessage) (ToolOutput, error) {
			ctx, release, err := restart.Child(ctx)
			if err != nil {
				return ToolOutput{}, err
			}
			defer release()
			tool, ok := e.tools.Get(name)
			if !ok {
				return ToolOutput{}, fmt.Errorf("tool not found: %s", name)
			}
			// Native provider bridges share the same actual-completion barrier as
			// engine-managed calls, including after their process is cancelled.
			if err := e.beginSteeringTool(ctx); err != nil {
				return ToolOutput{}, err
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
	return &StreamIncompleteError{Transport: "simulated stream", Terminal: "completion"}
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
func (e *Engine) RegisterTool(tool Tool) {
	e.tools.Register(tool)
}

// SetToolSurfacePlanner installs the planner that owns dynamic provider visibility.
func (e *Engine) SetToolSurfacePlanner(planner ToolSurfacePlanner) {
	e.toolPlannerMu.Lock()
	e.toolPlanner = planner
	e.toolPlannerMu.Unlock()
}

// ClearToolSurfacePlanner removes planner only when it still owns this engine.
// This prevents an old planner from detaching a newer replacement.
func (e *Engine) ClearToolSurfacePlanner(planner ToolSurfacePlanner) bool {
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

func (e *Engine) currentToolPlanner() ToolSurfacePlanner {
	e.toolPlannerMu.RLock()
	defer e.toolPlannerMu.RUnlock()
	return e.toolPlanner
}

// ToolDiscoveryDiagnostics returns current planner diagnostics when available.
func (e *Engine) ToolDiscoveryDiagnostics(sessionID string) (ToolDiscoveryDiagnostics, bool) {
	planner := e.currentToolPlanner()
	diagnoser, ok := planner.(ToolDiscoveryDiagnoser)
	if !ok {
		return ToolDiscoveryDiagnostics{}, false
	}
	return diagnoser.Diagnostics(sessionID), true
}

// ToolDiscoveryActiveSpecs returns the currently active MCP schemas for a
// session. It excludes catalogue entries that remain deferred.
func (e *Engine) ToolDiscoveryActiveSpecs(sessionID string) []ToolSpec {
	planner := e.currentToolPlanner()
	inspector, ok := planner.(ToolDiscoverySurfaceInspector)
	if !ok {
		return nil
	}
	return inspector.ActiveToolSpecs(sessionID)
}

// AddDynamicTool registers a tool and queues it for the currently active run.
// Calls outside a run only register the tool; durable activation belongs to the planner/tool.
func (e *Engine) AddDynamicTool(tool Tool) {
	e.tools.Register(tool)
	e.pendingToolsMu.Lock()
	runID := e.activeToolRunID
	e.pendingToolsMu.Unlock()
	if runID != "" {
		e.AddDynamicToolForRun(runID, tool)
	}
}

// AddDynamicToolForRun queues a provider schema only for the matching active run.
func (e *Engine) AddDynamicToolForRun(runID string, tool Tool) bool {
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
		pending = make(map[string]ToolSpec)
		e.pendingToolSpecs[runID] = pending
	}
	pending[spec.Name] = spec
	e.pendingToolsMu.Unlock()

	if publisher, ok := e.provider.(DynamicToolPublisher); ok {
		if err := publisher.PublishDynamicTools([]ToolSpec{spec}); err != nil {
			// Keep the normal pending queue as a fallback for the next provider
			// turn, but make the inline publication failure diagnosable.
			slog.Warn("publish dynamic tool to active provider", "tool", spec.Name, "run_id", runID, "error", err)
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
		slog.Warn("record file tracking run start", "session_id", sessionID, "run_id", runID, "error", err)
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
		slog.Warn("record file tracking run completion", "session_id", sessionID, "run_id", runID, "error", err)
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
	e.pendingToolSpecs = make(map[string]map[string]ToolSpec)
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

func (e *Engine) clearInlineFlush() {
	if resetter, ok := e.provider.(inlineFlushResetter); ok {
		resetter.clearInlineFlush()
	}
}

// drainPendingToolSpecs returns queued specs for one run and clears that queue.
func (e *Engine) drainPendingToolSpecs(runID string) []ToolSpec {
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
	specs := make([]ToolSpec, 0, len(names))
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

func resetProviderConversation(provider Provider) {
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
	e.pendingToolSpecs = make(map[string]map[string]ToolSpec)
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
func (e *Engine) SetDebugLogger(logger *DebugLogger) {
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
func (e *Engine) FilterAllowedToolSpecs(specs []ToolSpec) []ToolSpec {
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
	filtered := make([]ToolSpec, 0, len(specs))
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

func callResponseCompletedCallback(ctx context.Context, cb ResponseCompletedCallback, turnIndex int, assistantMsg Message, metrics TurnMetrics) bool {
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
func turnMessagesAfterResponseCallback(responseHandled bool, assistantMsg Message, rest []Message) []Message {
	if responseHandled {
		return rest
	}
	messages := make([]Message, 0, 1+len(rest))
	messages = append(messages, assistantMsg)
	messages = append(messages, rest...)
	return messages
}

// SetCompaction enables context compaction with the given input token limit
// and configuration. Only enable for models with known input limits.
// Must be called before Stream() or between streams (not during).
func (e *Engine) SetCompaction(inputLimit int, config CompactionConfig) {
	e.callbackMu.Lock()
	e.inputLimit = inputLimit
	e.compactionConfig = &config
	e.callbackMu.Unlock()
}

// SetContextTracking enables token tracking without enabling compaction.
// Use this to track context fullness when auto_compact is disabled.
// Must be called before Stream() or between streams (not during).
func (e *Engine) SetContextTracking(inputLimit int) {
	e.callbackMu.Lock()
	e.inputLimit = inputLimit
	e.compactionConfig = nil
	e.callbackMu.Unlock()
}

// ConfigureContextManagement enables compaction or context tracking based on
// the provider/model's input limit and the autoCompact setting.
// Providers that manage their own context, or models without a known limit,
// clear engine-side tracking/compaction to avoid leaking stale settings.
// Both inputLimit and compactionConfig are set atomically under a single lock.
func (e *Engine) ConfigureContextManagement(provider Provider, providerName, modelName string, autoCompact bool) {
	limit := 0
	var err error
	var compactionConfig *CompactionConfig

	if provider != nil && !provider.Capabilities().ManagesOwnContext {
		runtimeProvider := provider
		if retry, ok := provider.(*RetryProvider); ok {
			runtimeProvider = retry.inner
		}
		if limiter, ok := runtimeProvider.(interface {
			effectiveInputLimit(context.Context, string) (int, error)
		}); ok {
			// Runtime-aware providers must win over canonical model-prefix tables.
			// Ollama's GGUF metadata can advertise a much larger architectural
			// context than the live Modelfile allocates.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if scoped, ok := runtimeProvider.(interface {
				effectiveInputLimitForProvider(context.Context, string, string) (int, error)
			}); ok {
				limit, err = scoped.effectiveInputLimitForProvider(ctx, providerName, modelName)
			} else {
				limit, err = limiter.effectiveInputLimit(ctx, modelName)
			}
			cancel()
			if err != nil {
				slog.Debug("failed to inspect provider runtime context limit", "provider", providerName, "model", modelName, "error", err)
				limit = InputLimitForProviderModel(providerName, modelName)
				if limit == 0 {
					refreshDynamicModelLimitsForContext(provider, providerName, modelName)
					limit = InputLimitForProviderModel(providerName, modelName)
				}
			}
		} else {
			limit = InputLimitForProviderModel(providerName, modelName)
			if limit == 0 {
				refreshDynamicModelLimitsForContext(provider, providerName, modelName)
				limit = InputLimitForProviderModel(providerName, modelName)
			}
		}
		if limit > 0 && autoCompact {
			cfg := DefaultCompactionConfig()
			compactionConfig = &cfg
		}
	}

	e.callbackMu.Lock()
	e.inputLimit = limit
	e.compactionConfig = compactionConfig
	e.callbackMu.Unlock()
}

func refreshDynamicModelLimitsForContext(provider Provider, providerName, modelName string) {
	providerType := resolveProviderType(providerName)
	if (providerType != "copilot" && providerType != "opencode-go") || strings.TrimSpace(modelName) == "" {
		return
	}
	timeout := 30 * time.Second
	if providerType == "opencode-go" {
		timeout = opencodeGoRefreshTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if providerType == "opencode-go" {
		refresher, ok := provider.(interface {
			RefreshModelMetadata(context.Context) error
		})
		if !ok {
			return
		}
		if err := refresher.RefreshModelMetadata(ctx); err != nil {
			slog.Debug("failed to refresh dynamic model metadata for context limits", "provider", providerName, "model", modelName, "error", err)
		}
		return
	}
	lister, ok := provider.(interface {
		ListModels(context.Context) ([]ModelInfo, error)
	})
	if !ok {
		return
	}
	models, err := lister.ListModels(ctx)
	if err != nil {
		slog.Debug("failed to refresh dynamic model metadata for context limits", "provider", providerName, "model", modelName, "error", err)
		return
	}
	if providerType == "copilot" {
		RefreshCopilotCacheSync(models)
	}
}

// InputLimit returns the configured input token limit (0 if unknown).
func (e *Engine) InputLimit() int {
	e.callbackMu.RLock()
	v := e.inputLimit
	e.callbackMu.RUnlock()
	return v
}

// CompactionThresholds returns the configured soft and hard compaction token
// thresholds. The final value is false when compaction is disabled or the
// input limit is unknown.
func (e *Engine) CompactionThresholds() (soft, hard int, enabled bool) {
	e.callbackMu.RLock()
	inputLimit := e.inputLimit
	var config *CompactionConfig
	if e.compactionConfig != nil {
		copy := *e.compactionConfig
		config = &copy
	}
	e.callbackMu.RUnlock()

	if config == nil || inputLimit <= 0 {
		return 0, 0, false
	}
	softRatio, hardRatio := effectiveCompactionThresholdRatios(config)
	return int(float64(inputLimit) * softRatio), int(float64(inputLimit) * hardRatio), true
}

// LastTotalTokens returns the total tokens (cached+input+output) from the most
// recent API response, approximating current context fullness.
func (e *Engine) LastTotalTokens() int {
	e.callbackMu.RLock()
	v := e.lastTotalTokens
	e.callbackMu.RUnlock()
	return v
}

// ContextEstimateBaseline returns the persisted context-estimate baseline: the
// last observed exact total tokens plus a legacy message count retained for
// compatibility with existing session metadata.
func (e *Engine) ContextEstimateBaseline() (int, int) {
	e.callbackMu.RLock()
	total := e.lastTotalTokens
	count := e.lastMessageCount
	e.callbackMu.RUnlock()
	return total, count
}

// SetContextEstimateBaseline seeds the context-estimate baseline, typically
// from persisted session state on resume. The message count is legacy metadata;
// EstimateTokens recomputes the delta boundary from the transcript shape.
func (e *Engine) SetContextEstimateBaseline(lastTotalTokens, lastMessageCount int) {
	if lastTotalTokens < 0 {
		lastTotalTokens = 0
	}
	if lastMessageCount < 0 || lastTotalTokens == 0 {
		lastMessageCount = 0
	}
	e.callbackMu.Lock()
	e.lastTotalTokens = lastTotalTokens
	e.lastMessageCount = lastMessageCount
	e.callbackMu.Unlock()
}

// EstimateTokens returns the estimated input token count for the next API call
// based on the current message list. When a provider usage baseline is available,
// it treats that exact total as covering the transcript through the last assistant
// message and adds only the structural delta appended after that assistant turn.
func (e *Engine) EstimateTokens(messages []Message) int {
	e.callbackMu.RLock()
	lastTotalTokens := e.lastTotalTokens
	e.callbackMu.RUnlock()

	if lastTotalTokens <= 0 {
		return EstimateMessageTokens(messages)
	}

	afterLastAssistant := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleAssistant {
			afterLastAssistant = i + 1
			break
		}
	}
	if afterLastAssistant < 0 {
		// A persisted baseline is only valid when it can be anchored to an
		// assistant turn in the current transcript. Summary-only / cleared
		// contexts should not inherit a stale pre-compaction baseline.
		return EstimateMessageTokens(messages)
	}

	return lastTotalTokens + EstimateMessageTokens(messages[afterLastAssistant:])
}

// SetCompactionCallback sets the callback for context compaction events.
// Thread-safe: can be called while streaming is in progress.
func (e *Engine) SetCompactionCallback(cb CompactionCallback) {
	e.callbackMu.Lock()
	e.onCompaction = cb
	e.callbackMu.Unlock()
}

// SetMaxToolOutputChars sets the global maximum characters for tool output.
// Tool results exceeding this limit are truncated with head+tail preservation.
// Pass 0 to disable global truncation.
func (e *Engine) SetMaxToolOutputChars(n int) {
	e.callbackMu.Lock()
	e.maxToolOutputChars = n
	e.callbackMu.Unlock()
}

// QueueRequestModelSwitch requests a same-provider model change for the next
// provider turn in an active agentic loop. This is intended for reasoning-effort
// suffix changes while tools are running: the Engine cannot be replaced safely
// mid-stream, but req.Model can be updated before the next provider.Stream call.
func (e *Engine) QueueRequestModelSwitch(model string) {
	e.QueueRequestRuntimeSwitch(model, "")
}

// QueueRequestRuntimeSwitch requests a same-provider model/effort change for
// the next provider turn in an active agentic loop.
func (e *Engine) QueueRequestRuntimeSwitch(model, reasoningEffort string) {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	e.pendingRequestRuntime = pendingRequestRuntimeSwitch{
		model:           strings.TrimSpace(model),
		reasoningEffort: strings.TrimSpace(reasoningEffort),
	}
}

// QueueRequestServiceTier changes the service tier at the next provider request
// boundary without modifying an in-flight request. An empty tier explicitly
// clears the provider default. Safe to call while the engine is streaming.
func (e *Engine) QueueRequestServiceTier(tier string) {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	e.pendingServiceTier = &tier
}

// ClearPendingRequestServiceTier discards an override left by a completed run.
// Callers starting a new run should put their current preference in Request.
func (e *Engine) ClearPendingRequestServiceTier() {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	e.pendingServiceTier = nil
}

func (e *Engine) applyPendingServiceTier(req *Request) {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.pendingServiceTier != nil {
		req.ServiceTier = *e.pendingServiceTier
		req.ServiceTierSet = true
		e.pendingServiceTier = nil
	}
}

// ClearPendingRequestModelSwitch cancels any queued same-provider model change.
func (e *Engine) ClearPendingRequestModelSwitch() {
	e.QueueRequestRuntimeSwitch("", "")
}

func (e *Engine) drainPendingRequestRuntimeSwitch() pendingRequestRuntimeSwitch {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	pending := pendingRequestRuntimeSwitch{
		model:           strings.TrimSpace(e.pendingRequestRuntime.model),
		reasoningEffort: strings.TrimSpace(e.pendingRequestRuntime.reasoningEffort),
	}
	e.pendingRequestRuntime = pendingRequestRuntimeSwitch{}
	return pending
}

// Steer queues a text user message to be inserted after the current turn's tool results,
// right before the next LLM turn begins. Safe to call from any goroutine.
func (e *Engine) Steer(text string) {
	e.SteerWithID("", text)
}

// SteerWithID behaves like Steer but preserves a caller-supplied stable
// identifier.
func (e *Engine) SteerWithID(id, text string) {
	_ = e.QueueSteering(QueuedSteering{
		ID:          id,
		Message:     UserText(text),
		DisplayText: text,
	})
}

// QueueSteering appends a structured steering to the FIFO pending queue
// and returns its stable ID. The message role is normalized to RoleUser.
func (e *Engine) QueueSteering(entry QueuedSteering) string {
	id, _ := e.QueueSteeringWithStatus(entry)
	return id
}

// QueueSteeringWithStatus reports whether the stable identity was newly
// queued for an accepting agentic run, rejected because the current/latest run
// cannot consume it, already queued, transferred to a follow-up, or committed.
func (e *Engine) QueueSteeringWithStatus(entry QueuedSteering) (string, SteeringQueueStatus) {
	e.callbackMu.Lock()
	id, status := e.queueSteeringWithStatusLocked(entry)
	e.callbackMu.Unlock()
	if status == SteeringQueueQueued {
		e.requestInlineFlush()
	}
	return id, status
}

func (e *Engine) queueSteeringWithStatusLocked(entry QueuedSteering) (string, SteeringQueueStatus) {
	if e.steeringTransition != nil {
		return entry.ID, SteeringQueueTransitioning
	}

	entry.ID = strings.TrimSpace(entry.ID)
	if entry.ID == "" {
		entry.ID = nextEngineSteeringID()
	}
	if _, claimed := e.claimedSteeringIDs[entry.ID]; claimed {
		return entry.ID, SteeringQueueFollowUpOwned
	}
	if _, committed := e.committedSteeringIDs[entry.ID]; committed {
		return entry.ID, SteeringQueueCommitted
	}
	for i := range e.pendingSteering {
		if e.pendingSteering[i].ID == entry.ID {
			return entry.ID, SteeringQueueAlreadyQueued
		}
	}
	if e.steeringRunState == steeringRunNonConsuming {
		return entry.ID, SteeringQueueRunFinished
	}
	entry.Message.Role = RoleUser
	if strings.TrimSpace(entry.Message.ClientMessageID) == "" {
		entry.Message.ClientMessageID = entry.ID
	}
	if entry.DisplayText == "" {
		entry.DisplayText = MessageText(entry.Message)
		if strings.TrimSpace(entry.DisplayText) == "" {
			entry.DisplayText = MessageAttachmentSummary(entry.Message)
		}
	}
	entry.Status = SteeringQueued
	e.pendingSteering = append(e.pendingSteering, entry)
	return entry.ID, SteeringQueueQueued
}

// ClaimSteering atomically transfers queued steering to one normal
// follow-up request. IDs must be unique. If any ID is already committed or
// owned by another follow-up, no queued entry is removed.
func (e *Engine) ClaimSteering(ids []string) []SteeringClaimStatus {
	statuses := make([]SteeringClaimStatus, len(ids))
	if len(ids) == 0 {
		return statuses
	}
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.steeringTransition != nil {
		out := make([]SteeringClaimStatus, len(ids))
		for i := range out {
			out[i] = SteeringClaimRushOwned
		}
		return out
	}

	pending := make(map[string]struct{}, len(e.pendingSteering))
	for i := range e.pendingSteering {
		pending[e.pendingSteering[i].ID] = struct{}{}
	}
	claimable := true
	claimedIDs := make(map[string]struct{}, len(ids))
	for i, rawID := range ids {
		id := strings.TrimSpace(rawID)
		_, isPending := pending[id]
		_, isClaimed := e.claimedSteeringIDs[id]
		_, isCommitted := e.committedSteeringIDs[id]
		switch {
		case id == "":
			statuses[i] = SteeringClaimNotFound
		case isPending:
			statuses[i] = SteeringClaimed
			claimedIDs[id] = struct{}{}
		case isClaimed:
			statuses[i] = SteeringClaimFollowUpOwned
			claimable = false
		case isCommitted:
			statuses[i] = SteeringClaimCommitted
			claimable = false
		default:
			statuses[i] = SteeringClaimNotFound
		}
	}
	if !claimable || len(claimedIDs) == 0 {
		return statuses
	}

	kept := e.pendingSteering[:0]
	for i := range e.pendingSteering {
		entry := e.pendingSteering[i]
		if _, claimed := claimedIDs[entry.ID]; claimed {
			e.rememberClaimedSteeringIDLocked(entry.ID)
			continue
		}
		kept = append(kept, entry)
	}
	for i := len(kept); i < len(e.pendingSteering); i++ {
		e.pendingSteering[i] = QueuedSteering{}
	}
	e.pendingSteering = kept
	return statuses
}

// ClaimSteeringEntry atomically transfers a queued steering to a normal
// follow-up request. A committed result means the engine already drained the ID
// and the caller must not submit it again.
func (e *Engine) ClaimSteeringEntry(id string) SteeringClaimStatus {
	statuses := e.ClaimSteering([]string{id})
	if len(statuses) == 0 {
		return SteeringClaimNotFound
	}
	return statuses[0]
}

// CancelSteering removes a queued, not-yet-committed steering without
// transferring its identity to follow-up ownership.
func (e *Engine) CancelSteering(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.steeringTransition != nil {
		return false
	}
	for i := range e.pendingSteering {
		if e.pendingSteering[i].ID != id {
			continue
		}
		copy(e.pendingSteering[i:], e.pendingSteering[i+1:])
		e.pendingSteering[len(e.pendingSteering)-1] = QueuedSteering{}
		e.pendingSteering = e.pendingSteering[:len(e.pendingSteering)-1]
		return true
	}
	return false
}

// ReleaseClaimedSteering ends temporary follow-up ownership. Durable
// history remains authoritative for requests that reached persistence.
func (e *Engine) ReleaseClaimedSteering(ids []string) {
	if len(ids) == 0 {
		return
	}
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	released := make(map[string]struct{}, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		delete(e.claimedSteeringIDs, id)
		released[id] = struct{}{}
	}
	kept := e.claimedSteeringOrder[:0]
	for _, id := range e.claimedSteeringOrder {
		if _, drop := released[id]; !drop {
			kept = append(kept, id)
		}
	}
	for i := len(kept); i < len(e.claimedSteeringOrder); i++ {
		e.claimedSteeringOrder[i] = ""
	}
	e.claimedSteeringOrder = kept
}

// DiscardPendingSteering removes all queued, not-yet-committed
// steering and returns how many were discarded.
func (e *Engine) DiscardPendingSteering() int {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.steeringTransition != nil {
		return 0
	}
	count := len(e.pendingSteering)
	for i := range e.pendingSteering {
		e.pendingSteering[i] = QueuedSteering{}
	}
	e.pendingSteering = nil
	return count
}

// SteeringIdentityStatus reports engine ownership for a stable ID without
// mutating the queue. The boolean is false when the engine has never seen it.
func (e *Engine) SteeringIdentityStatus(id string) (SteeringQueueStatus, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", false
	}
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.steeringTransition != nil {
		for _, entry := range e.pendingSteering {
			if entry.ID == id {
				return SteeringQueueRushOwned, true
			}
		}
	}
	if _, claimed := e.claimedSteeringIDs[id]; claimed {
		return SteeringQueueFollowUpOwned, true
	}
	if _, committed := e.committedSteeringIDs[id]; committed {
		return SteeringQueueCommitted, true
	}
	for i := range e.pendingSteering {
		if e.pendingSteering[i].ID == id {
			return SteeringQueueAlreadyQueued, true
		}
	}
	return "", false
}

// ListPendingSteering returns a snapshot of queued, cancellable steering.
func (e *Engine) ListPendingSteering() []QueuedSteering {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.steeringTransition != nil {
		return nil
	}
	out := make([]QueuedSteering, len(e.pendingSteering))
	copy(out, e.pendingSteering)
	return out
}

// DrainSteeringText returns pending steering text and drains all queued
// steering. It is retained for legacy recovery paths; new callers should
// use DrainSteering or ListPendingSteering.
func (e *Engine) DrainSteeringText() string {
	entries := e.DrainSteering()
	var b strings.Builder
	for _, entry := range entries {
		text := entry.DisplayText
		if text == "" {
			text = MessageText(entry.Message)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	return b.String()
}

// DrainSteering drains all queued steering and marks returned entries
// committed. Draining is the atomic handoff after which cancellation fails.
func (e *Engine) DrainSteering() []QueuedSteering {
	return e.drainSteering()
}

// PeekSteering returns a text summary of currently pending steering.
func (e *Engine) PeekSteering() string {
	entries := e.ListPendingSteering()
	var b strings.Builder
	for _, entry := range entries {
		text := entry.DisplayText
		if text == "" {
			text = MessageText(entry.Message)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	return b.String()
}

func rememberBoundedSteeringID(ids map[string]struct{}, order []string, id string) (map[string]struct{}, []string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return ids, order
	}
	if ids == nil {
		ids = make(map[string]struct{})
	}
	if _, exists := ids[id]; exists {
		return ids, order
	}
	ids[id] = struct{}{}
	order = append(order, id)
	if len(order) <= steeringIdentityLimit {
		return ids, order
	}
	oldest := order[0]
	copy(order, order[1:])
	order[len(order)-1] = ""
	order = order[:len(order)-1]
	delete(ids, oldest)
	return ids, order
}

func (e *Engine) rememberClaimedSteeringIDLocked(id string) {
	e.claimedSteeringIDs, e.claimedSteeringOrder = rememberBoundedSteeringID(
		e.claimedSteeringIDs, e.claimedSteeringOrder, id,
	)
}

func (e *Engine) rememberCommittedSteeringIDLocked(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	delete(e.claimedSteeringIDs, id)
	e.committedSteeringIDs, e.committedSteeringOrder = rememberBoundedSteeringID(
		e.committedSteeringIDs, e.committedSteeringOrder, id,
	)
}

func (e *Engine) markSteeringCommittedLocked(entries []queuedSteering) {
	if len(entries) == 0 {
		return
	}
	for i := range entries {
		entries[i].Status = SteeringCommitted
		e.rememberCommittedSteeringIDLocked(entries[i].ID)
	}
}

// drainSteering atomically commits all queued steering.
func (e *Engine) drainSteering() []queuedSteering {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	return e.drainSteeringLocked()
}

func (e *Engine) drainSteeringForNextTurn(canAcceptMore bool) []queuedSteering {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	out := e.drainSteeringLocked()
	if !canAcceptMore {
		e.steeringRunState = steeringRunNonConsuming
	}
	return out
}

func (e *Engine) drainSteeringLocked() []queuedSteering {
	if e.steeringTransition != nil {
		return nil
	}
	if len(e.pendingSteering) == 0 {
		return nil
	}
	out := make([]queuedSteering, len(e.pendingSteering))
	copy(out, e.pendingSteering)
	e.markSteeringCommittedLocked(out)
	e.pendingSteering = nil
	return out
}

func (e *Engine) beginSteeringRun(canConsume bool) {
	e.callbackMu.Lock()
	if canConsume {
		e.steeringRunState = steeringRunAccepting
	} else {
		e.steeringRunState = steeringRunNonConsuming
	}
	e.callbackMu.Unlock()
}

func (e *Engine) markSteeringRunNonConsuming() {
	e.callbackMu.Lock()
	e.steeringRunState = steeringRunNonConsuming
	e.callbackMu.Unlock()
}

// drainBoundarySteering atomically either commits every pending steer for
// another provider turn or closes this run's final agentic boundary. Explicit
// error and cancellation exits are closed by runLoop's deferred teardown.
func (e *Engine) drainBoundarySteering(canContinue, canAcceptMore bool) []queuedSteering {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()

	if !canContinue || len(e.pendingSteering) == 0 {
		e.steeringRunState = steeringRunNonConsuming
		return nil
	}
	out := e.drainSteeringLocked()
	if !canAcceptMore {
		e.steeringRunState = steeringRunNonConsuming
	}
	return out
}

func (e *Engine) continueWithSteering(ctx context.Context, send eventSender, req *Request, turnCallback TurnCompletedCallback, attempt int, finalMsg Message, canContinue, canAcceptMore bool) (bool, error) {
	steering := e.drainBoundarySteering(canContinue, canAcceptMore)
	if len(steering) == 0 {
		return false, nil
	}
	if len(finalMsg.Parts) > 0 {
		req.Messages = append(req.Messages, finalMsg)
	}
	steeringMsgs := make([]Message, 0, len(steering))
	for _, steering := range steering {
		steeringMsg := steering.Message
		steeringMsg.Role = RoleUser
		req.Messages = append(req.Messages, steeringMsg)
		steeringMsgs = append(steeringMsgs, steeringMsg)
	}
	if turnCallback != nil {
		cbCtx, cancel := callbackContext(ctx)
		_ = turnCallback(cbCtx, attempt, steeringMsgs, TurnMetrics{})
		cancel()
	}
	for _, steering := range steering {
		text := steering.DisplayText
		if text == "" {
			text = MessageText(steering.Message)
		}
		if err := send.Send(Event{Type: EventSteering, Text: text, SteeringID: steering.ID, Message: steering.Message, SteeringStatus: SteeringCommitted}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// applyToolOutputTruncation applies global and compaction truncation limits
// to all textual tool output, including structured content parts. Global limit
// fires first (typically stricter), then compaction limit as a safety net.
func (e *Engine) applyToolOutputTruncation(output ToolOutput) ToolOutput {
	e.callbackMu.RLock()
	maxChars := e.maxToolOutputChars
	cc := e.compactionConfig
	e.callbackMu.RUnlock()

	if maxChars > 0 {
		output = truncateToolOutput(output, maxChars)
	}
	if cc != nil && cc.MaxToolResultChars > 0 {
		output = truncateToolOutput(output, cc.MaxToolResultChars)
	}
	return output
}

// getCompactionCallback returns the current compaction callback under read lock.
func (e *Engine) getCompactionCallback() CompactionCallback {
	e.callbackMu.RLock()
	cb := e.onCompaction
	e.callbackMu.RUnlock()
	return cb
}

// estimatedTokens returns the estimated input token count for the next API
// call. Uses total_tokens (input+output) from the last API response as the exact
// baseline through the last assistant turn, then adds heuristic estimates only
// for messages structurally appended after that assistant turn.
func (e *Engine) estimatedTokens(messages []Message) int {
	return e.EstimateTokens(messages)
}

// nonSystemMessages returns all messages that are not system messages.
func nonSystemMessages(messages []Message) []Message {
	var result []Message
	for _, msg := range messages {
		if msg.Role != RoleSystem {
			result = append(result, msg)
		}
	}
	return result
}

// IsToolAllowed checks if a tool can be executed under current restrictions.
func (e *Engine) IsToolAllowed(name string) bool {
	e.allowedMu.RLock()
	defer e.allowedMu.RUnlock()
	return e.allowedTools == nil || e.allowedTools[name]
}

const indirectVisionInstruction = "Uploaded images are represented as local file-path references for this text-only model. When visual content matters, call the view_image tool with the referenced file_path and, if useful, a focused question. Do not claim to have inspected an image unless you have called view_image or the user-provided text is sufficient."

func (e *Engine) prepareProviderRequest(req Request) Request {
	prepared := req
	prepared.Messages = providerSafeRequestMessages(req.Messages)
	if e.IndirectVision() {
		normalized := ensureIndirectVisionImagePaths(prepared.Messages)
		rewritten, changed := rewriteImagePartsAsReferences(normalized)
		prepared.Messages = rewritten
		if changed {
			prepared.Messages = prependIndirectVisionInstruction(prepared.Messages)
		}
		return prepared
	}
	prepared.Messages = hydrateImageDataFromPaths(prepared.Messages)
	return prepared
}

func providerSafeRequestMessages(messages []Message) []Message {
	var output []Message
	for index, message := range messages {
		needsRewrite := false
		for _, part := range message.Parts {
			if part.Type == PartConversationStart || part.Type == PartPlatformContext || part.Type == PartSkillActivation || part.Type == PartAgentMention || part.Type == PartDiffComment || part.Type == PartGoalSteering {
				needsRewrite = true
				break
			}
		}
		if !needsRewrite {
			if output != nil {
				output = append(output, message)
			}
			continue
		}
		if output == nil {
			output = append([]Message(nil), messages[:index]...)
		}
		copyMessage := message
		copyMessage.Parts = make([]Part, 0, len(message.Parts))
		for _, part := range message.Parts {
			switch part.Type {
			case PartConversationStart, PartPlatformContext, PartSkillActivation, PartDiffComment, PartGoalSteering:
				continue
			case PartAgentMention:
				part.Type = PartText
			}
			copyMessage.Parts = append(copyMessage.Parts, part)
		}
		if len(copyMessage.Parts) > 0 {
			output = append(output, copyMessage)
		}
	}
	if output == nil {
		return messages
	}
	return output
}

func ensureIndirectVisionImagePaths(messages []Message) []Message {
	out := make([]Message, len(messages))
	copy(out, messages)
	for i, msg := range messages {
		if msg.Role != RoleUser {
			continue
		}
		parts := make([]Part, len(msg.Parts))
		copy(parts, msg.Parts)
		changed := false
		for j, part := range parts {
			if part.Type != PartImage || isTermLLMUploadPath(part.ImagePath) {
				continue
			}
			path, ok := saveImageDataToUploads(part.ImageData)
			if !ok {
				continue
			}
			parts[j].ImagePath = path
			changed = true
		}
		if changed {
			out[i].Parts = parts
		}
	}
	return out
}

func saveImageDataToUploads(imageData *ToolImageData) (string, bool) {
	if imageData == nil || strings.TrimSpace(imageData.Base64) == "" {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(imageData.Base64))
	if err != nil || len(raw) == 0 {
		return "", false
	}
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		return "", false
	}
	uploadsDir := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0o700); err != nil {
		return "", false
	}
	ext := imageExtensionForMediaType(imageData.MediaType)
	sum := sha256.Sum256(raw)
	path := filepath.Join(uploadsDir, fmt.Sprintf("uploaded_image_%x%s", sum[:16], ext))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			return path, true
		}
		return "", false
	}
	return path, true
}

func imageExtensionForMediaType(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func hydrateImageDataFromPaths(messages []Message) []Message {
	out := make([]Message, len(messages))
	copy(out, messages)
	for i, msg := range messages {
		parts := make([]Part, len(msg.Parts))
		copy(parts, msg.Parts)
		changed := false
		for j, part := range parts {
			if part.Type != PartImage || strings.TrimSpace(part.ImagePath) == "" {
				continue
			}
			if part.ImageData != nil && strings.TrimSpace(part.ImageData.Base64) != "" {
				continue
			}
			data, ok := readHydratableImagePath(part.ImagePath)
			if !ok {
				parts[j] = Part{Type: PartText, Text: unavailableImageText(part.ImagePath)}
				changed = true
				continue
			}
			mediaType := ""
			if part.ImageData != nil {
				mediaType = strings.TrimSpace(part.ImageData.MediaType)
			}
			if mediaType == "" {
				mediaType = strings.TrimSpace(mime.TypeByExtension(strings.ToLower(filepath.Ext(part.ImagePath))))
			}
			if mediaType == "" {
				mediaType = "image/png"
			}
			imageData := ToolImageData{MediaType: mediaType, Base64: base64.StdEncoding.EncodeToString(data)}
			if part.ImageData != nil {
				imageData.Detail = part.ImageData.Detail
			}
			parts[j].ImageData = &imageData
			changed = true
		}
		if changed {
			out[i].Parts = parts
		}
	}
	return out
}

func readHydratableImagePath(path string) ([]byte, bool) {
	path = strings.TrimSpace(path)
	if path == "" || !isTermLLMUploadPath(path) {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

func isTermLLMUploadPath(path string) bool {
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		return false
	}
	uploadsDir := filepath.Join(dataDir, "uploads")
	uploadsDir, err = filepath.EvalSymlinks(uploadsDir)
	if err != nil {
		return false
	}
	path, err = filepath.EvalSymlinks(strings.TrimSpace(path))
	if err != nil {
		return false
	}
	if path == uploadsDir {
		return false
	}
	return strings.HasPrefix(path, uploadsDir+string(filepath.Separator))
}

func unavailableImageText(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "[image unavailable: saved file could not be read]"
	}
	return fmt.Sprintf("[image unavailable: saved file could not be read at %s]", path)
}

func prependIndirectVisionInstruction(messages []Message) []Message {
	for _, msg := range messages {
		if msg.Role == RoleDeveloper && strings.Contains(collectTextParts(msg.Parts), "Uploaded images are represented as local file-path references") {
			return messages
		}
	}
	instruction := Message{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: indirectVisionInstruction}}}
	insertAt := 0
	for insertAt < len(messages) && messages[insertAt].Role == RoleSystem {
		insertAt++
	}
	out := make([]Message, 0, len(messages)+1)
	out = append(out, messages[:insertAt]...)
	out = append(out, instruction)
	out = append(out, messages[insertAt:]...)
	return out
}

func rewriteImagePartsAsReferences(messages []Message) ([]Message, bool) {
	out := make([]Message, len(messages))
	changedAny := false
	for i, msg := range messages {
		out[i] = msg
		if msg.Role != RoleUser {
			continue
		}
		parts := make([]Part, 0, len(msg.Parts))
		changed := false
		for _, part := range msg.Parts {
			if part.Type != PartImage {
				parts = append(parts, part)
				continue
			}
			changed = true
			changedAny = true
			parts = append(parts, Part{Type: PartText, Text: imageReferenceText(part)})
		}
		if changed {
			out[i].Parts = parts
		}
	}
	return out, changedAny
}

func imageReferenceText(part Part) string {
	path := strings.TrimSpace(part.ImagePath)
	mediaType := ""
	if part.ImageData != nil {
		mediaType = strings.TrimSpace(part.ImageData.MediaType)
	}
	if path == "" {
		if mediaType == "" {
			return "[User uploaded an image, but no local file path is available for view_image.]"
		}
		return fmt.Sprintf("[User uploaded an image (%s), but no local file path is available for view_image.]", mediaType)
	}
	if mediaType == "" {
		return fmt.Sprintf("[User uploaded image: %s — use view_image with this file_path to inspect it.]", path)
	}
	return fmt.Sprintf("[User uploaded image: %s (%s) — use view_image with this file_path to inspect it.]", path, mediaType)
}

// PrepareCompactionContext lets available request-context tools attach
// request-only restoration state after durable compaction is generated. Callers
// use this for manual compaction paths; automatic engine compaction calls it
// with the already-filtered request specs.
func (e *Engine) PrepareCompactionContext(ctx context.Context, sessionID string, specs []ToolSpec, result *CompactionResult) error {
	if e == nil || e.provider == nil || result == nil || !e.provider.Capabilities().ToolCalls {
		return nil
	}
	specs = e.FilterAllowedToolSpecs(specs)
	for _, spec := range specs {
		tool, ok := e.tools.Get(spec.Name)
		if !ok {
			continue
		}
		contextTool, ok := tool.(RequestContextTool)
		if !ok {
			continue
		}
		if err := contextTool.PrepareCompactionContext(ctx, sessionID, result); err != nil {
			return fmt.Errorf("restore %s context after compaction: %w", spec.Name, err)
		}
	}
	return nil
}

func (e *Engine) prepareRequestContext(ctx context.Context, req *Request) {
	if e == nil || req == nil {
		return
	}
	for _, spec := range req.Tools {
		tool, ok := e.tools.Get(spec.Name)
		if !ok {
			continue
		}
		contextTool, ok := tool.(RequestContextTool)
		if !ok {
			continue
		}
		messages, err := contextTool.PrepareRequestContext(ctx, req.SessionID, req.Messages)
		if err != nil {
			// Restored tool state is an optional context enhancement. A stale or
			// temporarily unavailable state store must not prevent the request.
			slog.Warn("request context restoration failed; continuing without it", "tool", spec.Name, "error", err)
			continue
		}
		req.Messages = messages
	}
}

// Stream returns a stream, applying external tools when needed.
func (e *Engine) Stream(ctx context.Context, req Request) (Stream, error) {
	if resume := req.Resume; resume != nil {
		req = resume.Request
		req.Resume = resume
		if len(resume.ProviderState) > 0 {
			if importer, ok := e.provider.(ProviderStateImporter); ok {
				if err := importer.ImportProviderState(resume.ProviderState); err != nil {
					return nil, fmt.Errorf("restore provider continuation: %w", err)
				}
			}
		}
	}
	ctx = withDebugDiagnosticSink(ctx, e.debugLogger)
	// Until this request is proven agentic, it has no boundary at which it can
	// consume steering. This also clears accepting state left by a prior run.
	e.markSteeringRunNonConsuming()
	req.Messages = FilterConversationMessages(req.Messages)

	caps := e.provider.Capabilities()

	// 1. Handle external search/fetch tool injection
	// If Search is enabled, add web_search and read_url tools to the tool list.
	// The LLM will use them naturally during conversation like any other tool.
	if req.Search {
		needsExternalSearch := !caps.NativeWebSearch || req.ForceExternalSearch
		needsExternalFetch := (!caps.NativeWebFetch || req.ForceExternalSearch) && !req.DisableExternalWebFetch

		// Callers commonly pre-populate req.Tools from the full registry. Remove
		// external search tools when the provider owns that capability so native
		// search is exclusive rather than silently competing with (for example)
		// an Exa-backed MCP tool. Forced external search keeps the external tools.
		req.Tools = filterExternalSearchTools(req.Tools, needsExternalSearch, needsExternalFetch)

		if needsExternalSearch {
			if t, ok := e.tools.Get(WebSearchToolName); ok {
				if !hasToolNamed(req.Tools, WebSearchToolName) {
					req.Tools = append(req.Tools, t.Spec())
				}
			}
		}
		if needsExternalFetch {
			if t, ok := e.tools.Get(ReadURLToolName); ok {
				if !hasToolNamed(req.Tools, ReadURLToolName) {
					req.Tools = append(req.Tools, t.Spec())
				}
			}
		}
	}

	// Force external search means "do not use provider-native search".
	// Keep req.Search=true only for providers that must handle native search.
	if req.ForceExternalSearch && caps.NativeWebSearch {
		req.Search = false
	}

	// Keep the provider-visible tool surface aligned with execution policy. This
	// matters for explicit-empty skill filters and for restrictions activated
	// between agentic turns.
	req.Tools = e.FilterAllowedToolSpecs(req.Tools)
	var planner ToolSurfacePlanner
	if req.EnableToolDiscovery {
		planner = e.currentToolPlanner()
	}
	validateNamedChoice := func(choice ToolChoice) error {
		if choice.Mode != ToolChoiceName || hasToolNamed(req.Tools, choice.Name) {
			return nil
		}
		// The only missing forced name a planner may defer validation for is the
		// exact authorised MCP wrapper it can make visible at BeginRun.
		if planner != nil && planner.CanActivateDeferredTool(choice.Name) {
			return nil
		}
		return fmt.Errorf("selected tool %q is not allowed by the active tool filter", choice.Name)
	}
	if err := validateNamedChoice(req.ToolChoice); err != nil {
		return nil, err
	}
	if req.LastTurnToolChoice != nil {
		if err := validateNamedChoice(*req.LastTurnToolChoice); err != nil {
			return nil, err
		}
	}
	if len(req.Tools) == 0 && planner == nil {
		req.ToolChoice = ToolChoice{}
		req.LastTurnToolChoice = nil
	}

	// Restorable session context is capability-gated by the final filtered specs
	// and provider support. Requests without such a configured tool never touch
	// its controller or store.
	if caps.ToolCalls {
		e.prepareRequestContext(ctx, &req)
	}

	if req.DebugRaw {
		debugReq := e.prepareProviderRequest(req)
		DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), debugReq, "Request")
	}

	// 2. Decide if we use the agentic loop
	// We use it if request has tools, or this request opted into a discovery
	// planner that may add authorised MCP tools, and the provider supports calls.
	// Providers with a true mid-generation interrupt also need the loop when no
	// tools are exposed: the next provider turn is where the queued steer is
	// committed and delivered after the interrupted turn.
	useLoop := restart.CurrentTask(ctx) != nil || req.Resume != nil || (caps.ToolCalls && (len(req.Tools) > 0 || planner != nil || e.providerSupportsImmediateInterruption()))

	if useLoop {
		e.beginSteeringRun(getMaxTurns(req) > 1)
		if req.SessionID != "" {
			// Tools read this for session-scoped concerns like file-change tracking.
			ctx = ContextWithSessionID(ctx, req.SessionID)
		}
		stream := newEventStream(ctx, func(ctx context.Context, send eventSender) error {
			return e.runLoop(ctx, req, send)
		})
		stream = wrapLoggingStream(stream, e.provider.Name(), req.Model)
		stream = e.wrapDebugLoggingStream(stream)

		// Wrap with per-turn cleanup for providers that materialize temporary
		// prompt/image files. Conversation-scoped CleanupMCP is not invoked here;
		// it runs on runtime eviction or server shutdown.
		if cleaner, ok := e.provider.(ProviderTurnCleaner); ok {
			stream = &cleanupStream{inner: stream, cleanup: cleaner.CleanupTurn}
		}

		return stream, nil
	}

	// 3. Simple stream (no tools or no provider support for tools). Model output is
	// staged in an attempt-local scratchpad until the stream completes; if the
	// transport fails first, we can discard the scratchpad and replay safely.
	runID := e.beginToolRun()
	if req.SessionID != "" {
		ctx = ContextWithSessionID(ctx, req.SessionID)
	}
	ctx = ContextWithToolRunID(ctx, runID)
	if e.debugLogger != nil {
		debugReq := e.prepareProviderRequest(req)
		e.debugLogger.LogRequest(e.provider.Name(), req.Model, debugReq)
	}
	stream := newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		return e.runSimpleScratchpad(ctx, req, send)
	})
	stream = wrapLoggingStream(stream, e.provider.Name(), req.Model)
	stream = e.wrapDebugLoggingStream(stream)
	stream = &fileTrackingRunStream{inner: stream, start: func() {
		e.recordFileTrackingRunStart(ctx, req.SessionID, runID)
	}, complete: func() {
		e.recordFileTrackingRunComplete(ctx, req.SessionID, runID)
		e.endToolRun(runID)
	}}
	return stream, nil
}

type fileTrackingRunStream struct {
	inner     Stream
	start     func()
	complete  func()
	startOnce sync.Once
	once      sync.Once
}

func (s *fileTrackingRunStream) begin() {
	s.startOnce.Do(func() {
		if s.start != nil {
			s.start()
		}
	})
}
func (s *fileTrackingRunStream) finish() {
	s.once.Do(s.complete)
}
func (s *fileTrackingRunStream) Recv() (Event, error) {
	s.begin()
	event, err := s.inner.Recv()
	if err != nil || event.Type == EventDone || event.Type == EventError {
		s.finish()
	}
	return event, err
}
func (s *fileTrackingRunStream) Close() error {
	s.begin()
	err := s.inner.Close()
	s.finish()
	return err
}

// wrapCallbackStream wraps a stream to call the turn callback on completion.
// Used for simple (non-agentic) streams to enable incremental session saving.
func wrapCallbackStream(ctx context.Context, inner Stream, cb TurnCompletedCallback) Stream {
	return &callbackStream{
		inner:              inner,
		ctx:                ctx,
		text:               &strings.Builder{},
		reasoning:          &strings.Builder{},
		metrics:            TurnMetrics{},
		callback:           cb,
		reasoningItemID:    "",
		reasoningEncrypted: "",
		reasoningKind:      "",
	}
}

// callbackStream wraps a stream to accumulate text/usage and call callback on EOF.
type callbackStream struct {
	inner     Stream
	ctx       context.Context
	mu        sync.Mutex
	text      *strings.Builder
	reasoning *strings.Builder
	// reasoningTextItemID tracks only text-bearing items for display boundaries;
	// reasoningItemID also tracks metadata-only events for provider replay.
	reasoningTextItemID   string
	reasoningItemID       string
	reasoningEncrypted    string
	reasoningKind         ReasoningKind
	reasoningSummaryParts []string
	metrics               TurnMetrics
	callback              TurnCompletedCallback
	done                  bool
}

func (s *callbackStream) Recv() (Event, error) {
	event, err := s.inner.Recv()
	if err == io.EOF {
		// Call callback with accumulated content on normal completion
		s.fireCallback()
		return event, err
	}
	if err != nil {
		// Call callback on error too (best-effort save of partial output)
		s.fireCallback()
		return event, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Accumulate text and usage
	if event.Type == EventAttemptDiscard {
		s.text.Reset()
		s.reasoning.Reset()
		s.reasoningTextItemID = ""
		s.reasoningItemID = ""
		s.reasoningEncrypted = ""
		s.reasoningKind = ""
		s.reasoningSummaryParts = nil
		s.metrics = TurnMetrics{}
		return event, nil
	}
	if event.Type == EventTextDelta && event.Text != "" {
		s.text.WriteString(event.Text)
	}
	if event.Type == EventUsage && event.Use != nil {
		s.metrics.InputTokens += event.Use.InputTokens
		s.metrics.OutputTokens += event.Use.OutputTokens
		s.metrics.CachedInputTokens += event.Use.CachedInputTokens
		s.metrics.CacheWriteTokens += event.Use.CacheWriteTokens
	}
	if event.Type == EventReasoningDelta {
		internalreasoning.AppendStreamItemText(s.reasoning, &s.reasoningTextItemID, event.Text, event.ReasoningItemID)
		if event.Text != "" {
			s.reasoningKind = MergeReasoningKind(s.reasoningKind, event.ReasoningKind)
		}
		if len(event.ReasoningSummaryParts) > 0 {
			// Last-write-wins: Responses emits the full summary parts array on each delta.
			s.reasoningSummaryParts = append([]string(nil), event.ReasoningSummaryParts...)
			s.reasoningKind = MergeReasoningKind(s.reasoningKind, ReasoningKindSummary)
		}
		if event.ReasoningItemID != "" {
			s.reasoningItemID = event.ReasoningItemID
		}
		if event.ReasoningEncryptedContent != "" {
			s.reasoningEncrypted = event.ReasoningEncryptedContent
			s.reasoningKind = MergeReasoningKind(s.reasoningKind, event.ReasoningKind)
		}
	}

	return event, nil
}

// fireCallback invokes the callback once if there's accumulated content.
func (s *callbackStream) fireCallback() {
	var (
		cb      TurnCompletedCallback
		msg     Message
		metrics TurnMetrics
	)

	s.mu.Lock()
	if s.callback != nil && !s.done && (s.text.Len() > 0 || s.reasoning.Len() > 0 || len(s.reasoningSummaryParts) > 0 || s.reasoningItemID != "" || s.reasoningEncrypted != "") {
		reasoningText := s.reasoning.String()
		reasoningKind := ReasoningKind("")
		if reasoningText != "" || len(s.reasoningSummaryParts) > 0 || s.reasoningItemID != "" || s.reasoningEncrypted != "" {
			reasoningKind = NormalizeReasoningKind(s.reasoningKind)
		}
		if reasoningText == "" && len(s.reasoningSummaryParts) > 0 {
			reasoningText = strings.Join(s.reasoningSummaryParts, "\n\n")
		}
		reasoningTitle := ""
		if reasoningKind == ReasoningKindSummary {
			reasoningTitle = internalreasoning.ParseReasoningSummary(reasoningText).Title
		}
		s.done = true
		cb = s.callback
		msg = Message{
			Role: RoleAssistant,
			Parts: []Part{{
				Type:                      PartText,
				Text:                      s.text.String(),
				ReasoningContent:          reasoningText,
				ReasoningSummaryParts:     append([]string(nil), s.reasoningSummaryParts...),
				ReasoningItemID:           s.reasoningItemID,
				ReasoningEncryptedContent: s.reasoningEncrypted,
				ReasoningKind:             reasoningKind,
				ReasoningSummaryTitle:     reasoningTitle,
			}},
		}
		metrics = s.metrics
	}
	s.mu.Unlock()

	if cb != nil {
		cbCtx, cancel := callbackContext(s.ctx)
		defer cancel()
		_ = cb(cbCtx, 0, []Message{msg}, metrics)
	}
}

func (s *callbackStream) Close() error {
	// Best-effort: fire callback if stream closed without EOF/error
	s.fireCallback()
	return s.inner.Close()
}

func filterExternalSearchTools(tools []ToolSpec, keepSearch, keepFetch bool) []ToolSpec {
	filtered := make([]ToolSpec, 0, len(tools))
	for _, tool := range tools {
		switch tool.Name {
		case WebSearchToolName:
			if !keepSearch {
				continue
			}
		case ReadURLToolName:
			if !keepFetch {
				continue
			}
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

// hasToolNamed checks if a tool with the given name exists in the tool list.
func hasToolNamed(tools []ToolSpec, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func isUncommittedReplayableStreamError(err error) bool {
	if err == nil {
		return false
	}
	var incomplete *StreamIncompleteError
	if errors.As(err, &incomplete) {
		return true
	}
	var nonRecoverable *NonRecoverableStreamError
	return errors.As(err, &nonRecoverable)
}

func isCommittedStreamRecoveryError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var incomplete *StreamIncompleteError
	if errors.As(err, &incomplete) {
		return true
	}
	var nonRecoverable *NonRecoverableStreamError
	return errors.As(err, &nonRecoverable)
}

func (e *Engine) runSimpleScratchpad(ctx context.Context, req Request, send eventSender) error {
	turnCallback := e.getTurnCallback()
	snapshotCallback := e.getSnapshotCallback()
	var priorErr error
	for retry := 0; ; retry++ {
		e.applyPendingServiceTier(&req)
		providerReq := e.prepareProviderRequest(req)
		e.clearInlineFlush()
		stream, err := e.provider.Stream(ctx, providerReq)
		if err != nil {
			return err
		}

		var scratchpadHasDiscardableOutput bool
		var textBuilder strings.Builder
		var reasoningBuilder strings.Builder
		var reasoningTextItemID string
		var reasoningItemID string
		var reasoningEncryptedContent string
		var reasoningSummaryParts []string
		var reasoningKind ReasoningKind
		var providerReplayParts []Part
		var metrics TurnMetrics
		var failed error
		fireSnapshot := func() {
			if snapshotCallback == nil {
				return
			}
			msg := buildAssistantMessageWithReasoningMetadata(
				textBuilder.String(), nil, reasoningBuilder.String(), reasoningSummaryParts,
				reasoningItemID, reasoningEncryptedContent, reasoningKind,
			)
			msg = attachProviderReplayParts(msg, providerReplayParts)
			if len(msg.Parts) == 0 {
				return
			}
			cbCtx, cancel := callbackContext(ctx)
			_ = snapshotCallback(cbCtx, 0, msg)
			cancel()
		}

		for {
			if err := e.consumeChaosFailure(); err != nil {
				failed = err
				break
			}
			event, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				failed = err
				break
			}
			if event.Type == EventError && event.Err != nil {
				failed = event.Err
				break
			}
			if req.DebugRaw {
				DebugRawEvent(true, event)
			}
			switch event.Type {
			case EventTextDelta:
				if event.Text != "" {
					textBuilder.WriteString(event.Text)
				}
				scratchpadHasDiscardableOutput = true
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
				}
			case EventReasoningDelta:
				internalreasoning.AppendStreamItemText(&reasoningBuilder, &reasoningTextItemID, event.Text, event.ReasoningItemID)
				if event.Text != "" {
					reasoningKind = MergeReasoningKind(reasoningKind, event.ReasoningKind)
				}
				if event.ReasoningItemID != "" {
					reasoningItemID = event.ReasoningItemID
				}
				if len(event.ReasoningSummaryParts) > 0 {
					reasoningSummaryParts = append([]string(nil), event.ReasoningSummaryParts...)
					reasoningKind = MergeReasoningKind(reasoningKind, ReasoningKindSummary)
				}
				if event.ReasoningEncryptedContent != "" {
					reasoningEncryptedContent = event.ReasoningEncryptedContent
					reasoningKind = MergeReasoningKind(reasoningKind, event.ReasoningKind)
				}
				scratchpadHasDiscardableOutput = true
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
				}
			case EventUsage:
				if event.Use != nil {
					metrics.InputTokens += event.Use.InputTokens
					metrics.OutputTokens += event.Use.OutputTokens
					metrics.CachedInputTokens += event.Use.CachedInputTokens
					metrics.CacheWriteTokens += event.Use.CacheWriteTokens
				}
				scratchpadHasDiscardableOutput = true
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
				}
			case EventImageGenerated:
				scratchpadHasDiscardableOutput = true
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
				}
			case EventToolActivity:
				if event.ToolActivity != nil {
					providerReplayParts = upsertToolActivityPart(providerReplayParts, event.ToolActivity)
					scratchpadHasDiscardableOutput = true
					fireSnapshot()
				}
			case EventProviderReplay:
				if event.ProviderReplay != nil && len(event.ProviderReplay.Raw) > 0 {
					replay := &ProviderReplayItem{Raw: append(json.RawMessage(nil), event.ProviderReplay.Raw...)}
					providerReplayParts = append(providerReplayParts, Part{Type: PartProviderReplay, ProviderReplay: replay})
					scratchpadHasDiscardableOutput = true
				}
			case EventDone:
				// The engine emits one done event after committing the scratchpad.
			default:
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
				}
			}
		}
		_ = stream.Close()

		if failed != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			priorErr = failed
			if retry >= defaultUncommittedStreamMaxRetries || !isUncommittedReplayableStreamError(failed) {
				return failed
			}
			attempt := retry + 1
			if scratchpadHasDiscardableOutput {
				if err := send.Send(Event{Type: EventAttemptDiscard}); err != nil {
					return err
				}
			}
			if err := send.Send(Event{Type: EventRetry, RetryAttempt: attempt, RetryMaxAttempts: defaultUncommittedStreamMaxRetries, RetryWaitSecs: 0}); err != nil {
				return err
			}
			slog.Debug("retrying failed uncommitted model stream", "attempt", attempt, "error", failed)
			continue
		}

		if textBuilder.Len() == 0 && reasoningBuilder.Len() == 0 && len(reasoningSummaryParts) == 0 && reasoningItemID == "" && reasoningEncryptedContent == "" && len(providerReplayParts) == 0 && priorErr != nil {
			return priorErr
		}
		if turnCallback != nil && (textBuilder.Len() > 0 || reasoningBuilder.Len() > 0 || len(reasoningSummaryParts) > 0 || reasoningItemID != "" || reasoningEncryptedContent != "" || len(providerReplayParts) > 0) {
			reasoningText := reasoningBuilder.String()
			if reasoningText == "" && len(reasoningSummaryParts) > 0 {
				reasoningText = strings.Join(reasoningSummaryParts, "\n\n")
			}
			if reasoningText != "" || len(reasoningSummaryParts) > 0 || reasoningItemID != "" || reasoningEncryptedContent != "" {
				reasoningKind = NormalizeReasoningKind(reasoningKind)
			}
			reasoningTitle := ""
			if reasoningKind == ReasoningKindSummary {
				reasoningTitle = internalreasoning.ParseReasoningSummary(reasoningText).Title
			}
			finalMsg := Message{Role: RoleAssistant, Parts: []Part{{
				Type:                      PartText,
				Text:                      textBuilder.String(),
				ReasoningContent:          reasoningText,
				ReasoningSummaryParts:     append([]string(nil), reasoningSummaryParts...),
				ReasoningItemID:           reasoningItemID,
				ReasoningEncryptedContent: reasoningEncryptedContent,
				ReasoningKind:             reasoningKind,
				ReasoningSummaryTitle:     reasoningTitle,
			}}}
			finalMsg = attachProviderReplayParts(finalMsg, providerReplayParts)
			cbCtx, cancel := callbackContext(ctx)
			_ = turnCallback(cbCtx, 0, []Message{finalMsg}, metrics)
			cancel()
		}
		return send.Send(Event{Type: EventDone})
	}
}

func attachProviderReplayParts(msg Message, parts []Part) Message {
	seenActivities := make(map[string]struct{})
	for _, part := range parts {
		if part.Type != PartToolActivity || part.ToolActivity == nil {
			continue
		}
		activity := cloneToolActivity(part.ToolActivity)
		key := activity.ID + "\x00" + activity.Name
		if _, seen := seenActivities[key]; seen {
			continue
		}
		seenActivities[key] = struct{}{}
		msg.Parts = append(msg.Parts, Part{Type: PartToolActivity, ToolActivity: activity})
	}
	msg.Parts = append(msg.Parts, cloneProviderReplayParts(parts)...)
	return msg
}

func upsertToolActivityPart(parts []Part, activity *ToolActivity) []Part {
	if activity == nil {
		return parts
	}
	for i := range parts {
		if parts[i].Type == PartToolActivity && parts[i].ToolActivity != nil &&
			parts[i].ToolActivity.ID == activity.ID && parts[i].ToolActivity.Name == activity.Name {
			parts[i].ToolActivity = cloneToolActivity(activity)
			return parts
		}
	}
	return append(parts, Part{Type: PartToolActivity, ToolActivity: cloneToolActivity(activity)})
}

func cloneProviderReplayParts(parts []Part) []Part {
	if len(parts) == 0 {
		return nil
	}
	out := make([]Part, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case PartProviderReplay:
			if part.ProviderReplay != nil && len(part.ProviderReplay.Raw) > 0 {
				out = append(out, Part{Type: PartProviderReplay, ProviderReplay: &ProviderReplayItem{Raw: append(json.RawMessage(nil), part.ProviderReplay.Raw...)}})
			}
		case PartDiscoveryCall:
			if part.DiscoveryCall != nil {
				call := *part.DiscoveryCall
				call.Arguments = append(json.RawMessage(nil), part.DiscoveryCall.Arguments...)
				out = append(out, Part{Type: PartDiscoveryCall, DiscoveryCall: &call})
			}
		case PartDiscoveryOutput:
			if part.DiscoveryOutput != nil {
				out = append(out, Part{Type: PartDiscoveryOutput, DiscoveryOutput: cloneToolDiscoveryOutput(part.DiscoveryOutput)})
			}
		}
	}
	return out
}

func collectToolDiscoveryReplay(messages []Message) []Part {
	var replay []Part
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type != PartDiscoveryCall && part.Type != PartDiscoveryOutput {
				continue
			}
			if cloned, ok := clonePart(part); ok {
				replay = append(replay, cloned)
			}
		}
	}
	return replay
}

func restoreToolDiscoveryReplay(messages []Message, replay []Part) []Message {
	if len(replay) == 0 {
		return messages
	}
	cleaned := make([]Message, 0, len(messages)+1)
	for _, message := range messages {
		copyMessage := message
		copyMessage.Parts = copyMessage.Parts[:0]
		for _, part := range message.Parts {
			if part.Type != PartDiscoveryCall && part.Type != PartDiscoveryOutput {
				copyMessage.Parts = append(copyMessage.Parts, part)
			}
		}
		if len(copyMessage.Parts) > 0 {
			cleaned = append(cleaned, copyMessage)
		}
	}
	insertAt := len(cleaned)
	for i := len(cleaned) - 1; i >= 0; i-- {
		if cleaned[i].Role == RoleUser {
			insertAt = i
			break
		}
	}
	replayMessage := Message{Role: RoleAssistant, Parts: cloneParts(replay)}
	cleaned = append(cleaned, Message{})
	copy(cleaned[insertAt+1:], cleaned[insertAt:])
	cleaned[insertAt] = replayMessage
	return cleaned
}

func (e *Engine) runLoop(ctx context.Context, req Request, send eventSender) (returnErr error) {
	syncBridgeCtx, cancelSyncBridges := context.WithCancel(ctx)
	defer cancelSyncBridges()
	task := restart.CurrentTask(ctx)
	if req.Ephemeral || (task != nil && !task.Claim(e)) {
		task = nil
	}
	if task != nil && e.provider.Capabilities().InlineToolLoop {
		if flusher, ok := e.provider.(interface{ RequestReloadFlush() }); ok {
			task.OnRequest(func() {
				if task.Pending() {
					flusher.RequestReloadFlush()
				}
			})
			defer task.OnRequest(nil)
		}
	}
	defer task.Release(e)
	ctx, finishStep := task.Step(ctx)
	defer finishStep()
	resume := req.Resume
	req.Resume = nil
	nextTurn := 0
	baseMessageCount := len(req.Messages)
	providerInFlight := false
	if resume != nil {
		nextTurn = resume.Turn
		baseMessageCount = resume.BaseMessageCount
	}
	var checkpointErr error
	defer func() {
		var alreadySuspended *SuspendedError
		if returnErr == nil || errors.As(returnErr, &alreadySuspended) {
			return
		}
		if task.Pending() && interruptedForRestart(ctx) && !e.provider.Capabilities().InlineToolLoop {
			e.waitActualTools()
			if checkpointErr != nil {
				returnErr = checkpointErr
				return
			}
			returnErr = e.suspend(req, nextTurn, nil, true, baseMessageCount)
			var checkpoint *SuspendedError
			if providerInFlight && errors.As(returnErr, &checkpoint) {
				checkpoint.Continuation.DiscardPartial = true
				_ = send.Send(Event{Type: EventAttemptDiscard, ProviderTurnIndex: nextTurn, ProviderTurnIndexSet: true})
			}
		}
	}()
	ctx = withResponsesWebSocketContinuationLifetime(ctx)
	defer e.markSteeringRunNonConsuming()
	runID := e.beginToolRun()
	modelSwitchOrdinal := 0
	e.recordFileTrackingRunStart(ctx, req.SessionID, runID)
	defer func() {
		e.recordFileTrackingRunComplete(ctx, req.SessionID, runID)
		e.endToolRun(runID)
	}()
	ctx = ContextWithToolRunID(ctx, runID)
	var planner ToolSurfacePlanner
	if req.EnableToolDiscovery {
		planner = e.currentToolPlanner()
	}
	if planner != nil {
		defer planner.EndRun(runID)
		resetReason, err := planner.BeginRun(ctx, e.provider, &req, runID)
		if err != nil {
			return fmt.Errorf("prepare tool discovery: %w", err)
		}
		if resetReason != "" {
			resetProviderConversation(e.provider)
			slog.Debug("reset provider conversation for tool-surface change", "reason", resetReason, "session_id", req.SessionID)
		}
	}
	sendDone := func() error {
		e.markSteeringRunNonConsuming()
		return send.Send(Event{Type: EventDone})
	}
	maxTurns := getMaxTurns(req)
	originalToolChoice := req.ToolChoice
	originalTools := append([]ToolSpec(nil), req.Tools...)
	restoredToolChoice := false

	// Snapshot callbacks and compaction config at start — protects against
	// concurrent modification from the UI thread (e.g., SetCompaction called
	// from a new startStream while a previous stream is finishing).
	turnCallback := e.getTurnCallback()
	responseCallback := e.getResponseCallback()
	snapshotCallback := e.getSnapshotCallback()
	if task != nil {
		if persist := turnCallback; persist != nil {
			turnCallback = func(ctx context.Context, turn int, messages []Message, metrics TurnMetrics) error {
				err := persist(ctx, turn, messages, metrics)
				if err != nil && checkpointErr == nil {
					checkpointErr = err
				}
				return err
			}
		}
		if persist := responseCallback; persist != nil {
			responseCallback = func(ctx context.Context, turn int, message Message, metrics TurnMetrics) error {
				err := persist(ctx, turn, message, metrics)
				if err != nil && checkpointErr == nil {
					checkpointErr = err
				}
				return err
			}
		}
	}

	compaction := newRunCompactionController(ctx, e, &req, send)
	var recoveredToolWork bool
	var recoveredToolCallIDs map[string]bool
	var recoveredAtMessageCount = -1
	var recoveryPriorErr error
	var uncommittedStreamRetries int
	var uncommittedPriorErr error

	if resume != nil && len(resume.Pending) > 0 {
		if task.Pending() {
			return e.suspend(req, resume.Turn, resume.Pending, resume.Interrupted, baseMessageCount, resume.PendingMetrics)
		}
		nextTurn = resume.Turn + 1
		if err := e.resumePending(ctx, &req, resume, send, turnCallback); err != nil {
			return err
		}
		for _, call := range resume.Pending {
			name := call.Name
			if mapped := req.ToolMap[name]; mapped != "" {
				name = mapped
			}
			if e.tools.IsFinishingTool(name) {
				return sendDone()
			}
		}
	}
	var activeSyncTools *syncToolSupervisor
	defer func() {
		if activeSyncTools == nil {
			return
		}
		cause := returnErr
		if cause == nil {
			cause = context.Canceled
		}
		activeSyncTools.abort(cause)
	}()

	for attempt := nextTurn; attempt < maxTurns; attempt++ {
		nextTurn = attempt
		turnErr := e.runProviderTurn(&engineRunTurnContext{
			ctx: ctx, req: &req, send: send, syncBridgeCtx: syncBridgeCtx, task: task,
			checkpointErr: &checkpointErr, baseMessageCount: baseMessageCount, maxTurns: maxTurns,
			planner: planner, runID: runID, modelSwitchOrdinal: &modelSwitchOrdinal,
			originalToolChoice: originalToolChoice, originalTools: &originalTools, restoredToolChoice: &restoredToolChoice,
			turnCallback: turnCallback, responseCallback: responseCallback, snapshotCallback: snapshotCallback,
			compaction: compaction, recoveredToolWork: &recoveredToolWork, recoveredToolCallIDs: &recoveredToolCallIDs,
			recoveredAtMessageCount: &recoveredAtMessageCount, recoveryPriorErr: &recoveryPriorErr,
			uncommittedStreamRetries: &uncommittedStreamRetries, uncommittedPriorErr: &uncommittedPriorErr,
			activeSyncTools: &activeSyncTools, nextTurn: &nextTurn, providerInFlight: &providerInFlight,
			sendDone: sendDone,
		}, attempt)
		if errors.Is(turnErr, errEngineTurnRetry) {
			attempt--
			continue
		}
		if errors.Is(turnErr, errEngineTurnAdvance) {
			continue
		}
		return turnErr
	}

	return fmt.Errorf("agentic loop ended unexpectedly")
}

// buildAssistantMessage creates an assistant message with text, tool calls, and optional reasoning.
// The reasoning parameter is for thinking models (OpenRouter reasoning_content).
func buildAssistantMessage(text string, toolCalls []ToolCall, reasoning string) Message {
	return buildAssistantMessageWithReasoningMetadata(text, toolCalls, reasoning, nil, "", "", ReasoningKindUnknown)
}

func buildAssistantMessageWithReasoningMetadata(text string, toolCalls []ToolCall, reasoning string, reasoningSummaryParts []string, reasoningItemID, reasoningEncryptedContent string, reasoningKind ReasoningKind) Message {
	var parts []Part
	if text != "" || reasoning != "" || len(reasoningSummaryParts) > 0 || reasoningItemID != "" || reasoningEncryptedContent != "" {
		if reasoning == "" && len(reasoningSummaryParts) > 0 {
			reasoning = strings.Join(reasoningSummaryParts, "\n\n")
		}
		hasReasoningMetadata := reasoning != "" || len(reasoningSummaryParts) > 0 || reasoningItemID != "" || reasoningEncryptedContent != ""
		if hasReasoningMetadata {
			reasoningKind = NormalizeReasoningKind(reasoningKind)
		} else {
			reasoningKind = ""
		}
		reasoningTitle := ""
		if reasoningKind == ReasoningKindSummary {
			reasoningTitle = internalreasoning.ParseReasoningSummary(reasoning).Title
		}
		parts = append(parts, Part{
			Type:                      PartText,
			Text:                      text,
			ReasoningContent:          reasoning,
			ReasoningSummaryParts:     append([]string(nil), reasoningSummaryParts...),
			ReasoningItemID:           reasoningItemID,
			ReasoningEncryptedContent: reasoningEncryptedContent,
			ReasoningKind:             reasoningKind,
			ReasoningSummaryTitle:     reasoningTitle,
		})
	}
	for i := range toolCalls {
		call := toolCalls[i]
		parts = append(parts, Part{Type: PartToolCall, ToolCall: &call})
	}
	return Message{Role: RoleAssistant, Parts: parts}
}

func buildInterleavedAssistantMessageWithReasoningMetadata(orderedParts []Part, reasoning string, reasoningSummaryParts []string, reasoningItemID, reasoningEncryptedContent string, reasoningKind ReasoningKind) Message {
	parts := append([]Part(nil), orderedParts...)
	metadata := buildAssistantMessageWithReasoningMetadata("", nil, reasoning, reasoningSummaryParts, reasoningItemID, reasoningEncryptedContent, reasoningKind)
	if len(metadata.Parts) == 0 {
		return Message{Role: RoleAssistant, Parts: parts}
	}

	metadataPart := metadata.Parts[0]
	for i := range parts {
		if parts[i].Type != PartText {
			continue
		}
		parts[i].ReasoningContent = metadataPart.ReasoningContent
		parts[i].ReasoningSummaryParts = metadataPart.ReasoningSummaryParts
		parts[i].ReasoningItemID = metadataPart.ReasoningItemID
		parts[i].ReasoningEncryptedContent = metadataPart.ReasoningEncryptedContent
		parts[i].ReasoningKind = metadataPart.ReasoningKind
		parts[i].ReasoningSummaryTitle = metadataPart.ReasoningSummaryTitle
		return Message{Role: RoleAssistant, Parts: parts}
	}

	parts = append([]Part{metadataPart}, parts...)
	return Message{Role: RoleAssistant, Parts: parts}
}

// buildApprovalTranscript assembles the policy-review evidence a tool approval
// reviewer sees: review-only prefix (e.g. a parent agent's transcript), the
// durable conversation, the in-progress assistant turn that requested the tool,
// and any tool results already produced this turn. Guardian denies when it finds
// no real operator message here, so every tool-execution path must build it.
//
// results are appended after the assistant message rather than interleaved with
// the calls that produced them. Providers with an inline tool loop (grok-bin,
// cursor-bin) can therefore present the Nth review with one assistant message
// listing calls 1..N followed by results 1..N-1, which reads as a single planned
// batch rather than a call/result chain. All evidence is present and guardian
// stamps each entry with an index, but the causal link between a tool result and
// a command it induced is flattened. Restoring it needs per-call assistant
// segments, which the non-ordered path cannot currently reconstruct.
func buildApprovalTranscript(prefix, messages []Message, assistant Message, results ...Message) []Message {
	transcript := make([]Message, 0, len(prefix)+len(messages)+1+len(results))
	transcript = appendApprovalEvidence(transcript, prefix...)
	transcript = appendApprovalEvidence(transcript, messages...)
	if stripped := withoutProviderReplayParts(assistant); len(stripped.Parts) > 0 {
		transcript = append(transcript, stripped)
	}
	return appendApprovalEvidence(transcript, results...)
}

func appendApprovalEvidence(dst []Message, messages ...Message) []Message {
	for _, msg := range messages {
		dst = append(dst, withoutProviderReplayParts(msg))
	}
	return dst
}

// withoutProviderReplayParts drops opaque provider protocol state from a message
// destined for policy review. Replay parts carry encrypted/provider-private
// payloads that exist only to be echoed back to the provider; they are never
// rendered as review evidence and must not reach a reviewer. Stripping happens
// here rather than at each caller because durable history, a parent agent's
// prefix, and the in-progress assistant turn can all carry replay parts, and
// only this choke point sees every source. The returned message shares the
// original parts; the input is never mutated.
func withoutProviderReplayParts(msg Message) Message {
	replays := 0
	for _, part := range msg.Parts {
		if part.Type == PartProviderReplay {
			replays++
		}
	}
	if replays == 0 {
		return msg
	}
	parts := make([]Part, 0, len(msg.Parts)-replays)
	for _, part := range msg.Parts {
		if part.Type == PartProviderReplay {
			continue
		}
		parts = append(parts, part)
	}
	msg.Parts = parts
	return msg
}

// toolCallOutcome is the provider-neutral result of one engine-owned tool
// execution. Both API providers and synchronous CLI bridges adapt this same
// value to their respective result protocols.
type toolCallOutcome struct {
	call   ToolCall
	output ToolOutput
	err    error
}

func (o toolCallOutcome) message() Message {
	if o.err != nil {
		return toolErrorMessageWithGuardian(o.call.ID, o.call.Name, fmt.Sprintf("Error: %v", o.err), o.call.ThoughtSig, o.output.GuardianReviews)
	}
	return ToolResultMessageFromOutput(o.call.ID, o.call.Name, o.output, o.call.ThoughtSig)
}

// executeToolCallOutcomes executes one model-authored batch. Outcomes retain
// dispatch order even when workers finish out of order. Cancellation returns
// promptly with a terminal outcome for every announced call.
func (e *Engine) executeToolCallOutcomes(ctx context.Context, calls []ToolCall, parallel bool, send eventSender, debug bool, debugRaw bool, transcript []Message) ([]toolCallOutcome, error) {
	outcomes := make([]toolCallOutcome, len(calls))
	if err := ctx.Err(); err != nil {
		for i, call := range calls {
			outcomes[i] = toolCallOutcome{call: call, err: err}
		}
		return outcomes, nil
	}
	if len(calls) == 0 {
		return outcomes, nil
	}

	toolCtx := ContextWithApprovalTranscript(ctx, transcript)
	if len(calls) == 1 || !parallel {
		for i, call := range calls {
			if err := ctx.Err(); err != nil {
				for j := i; j < len(calls); j++ {
					outcomes[j] = toolCallOutcome{call: calls[j], err: err}
				}
				break
			}
			outcomes[i] = e.executeSingleToolCallOutcomeSafe(toolCtx, call, send, debug, debugRaw)
		}
		return outcomes, nil
	}

	type indexedOutcome struct {
		index   int
		outcome toolCallOutcome
	}
	resultChan := make(chan indexedOutcome, len(calls))
	workerCount := maxParallelToolWorkers(len(calls))
	var nextCall atomic.Uint32
	for worker := 0; worker < workerCount; worker++ {
		workerCtx, release, err := restart.Child(toolCtx)
		if err != nil {
			return nil, err
		}
		go func() {
			defer release()
			for {
				if ctx.Err() != nil {
					return
				}
				idx := int(nextCall.Add(1)) - 1
				if idx >= len(calls) {
					return
				}
				if ctx.Err() != nil {
					return
				}
				resultChan <- indexedOutcome{index: idx, outcome: e.executeSingleToolCallOutcomeSafe(workerCtx, calls[idx], send, debug, debugRaw)}
			}
		}()
	}

	for remaining := len(calls); remaining > 0; remaining-- {
		select {
		case result := <-resultChan:
			outcomes[result.index] = result.outcome
		case <-ctx.Done():
			for drained := false; !drained; {
				select {
				case result := <-resultChan:
					outcomes[result.index] = result.outcome
				default:
					drained = true
				}
			}
			for i := range outcomes {
				if outcomes[i].call.ID == "" {
					outcomes[i] = toolCallOutcome{call: calls[i], err: ctx.Err()}
				}
			}
			return outcomes, nil
		}
	}
	return outcomes, nil
}

// executeToolCalls executes multiple tool calls, potentially in parallel.
func (e *Engine) executeToolCalls(ctx context.Context, calls []ToolCall, parallel bool, send eventSender, debug bool, debugRaw bool, transcriptOpt ...[]Message) ([]Message, error) {
	var transcript []Message
	if len(transcriptOpt) > 0 {
		transcript = transcriptOpt[0]
	}
	outcomes, err := e.executeToolCallOutcomes(ctx, calls, parallel, send, debug, debugRaw, transcript)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(outcomes))
	for i, outcome := range outcomes {
		messages[i] = outcome.message()
	}
	return messages, nil
}

// executeSingleToolCallOutcomeSafe wraps execution with panic recovery.
func (e *Engine) executeSingleToolCallOutcomeSafe(ctx context.Context, call ToolCall, send eventSender, debug bool, debugRaw bool) (outcome toolCallOutcome) {
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("tool panicked: %v", r)
			sendToolExecEnd(send, Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolSuccess: false})
			outcome = toolCallOutcome{call: call, err: err}
		}
	}()
	return e.executeSingleToolCallOutcome(ctx, call, send, debug, debugRaw)
}

// executeSingleToolCallSafe is retained as the message adapter used by callers
// that execute one call outside a complete batch.
func (e *Engine) executeSingleToolCallSafe(ctx context.Context, call ToolCall, send eventSender, debug bool, debugRaw bool) ([]Message, error) {
	outcome := e.executeSingleToolCallOutcomeSafe(ctx, call, send, debug, debugRaw)
	return []Message{outcome.message()}, nil
}

type toolExecutionResult struct {
	output     ToolOutput
	err        error
	panicValue any
}

// executeToolWithCancellation isolates Tool.Execute so a tool that ignores its
// context cannot keep the engine blocked after the caller cancels. The buffered
// result channel lets an abandoned invocation finish without blocking later.
func (e *Engine) executeToolWithCancellation(ctx context.Context, tool Tool, args json.RawMessage) (ToolOutput, error, any) {
	ctx, release, err := restart.Child(ctx)
	if err != nil {
		return ToolOutput{}, err, nil
	}
	if err := e.beginSteeringTool(ctx); err != nil {
		release()
		return ToolOutput{}, err, nil
	}

	results := make(chan toolExecutionResult, 1)
	go func() {
		defer release()
		defer e.actualSteeringToolDone()
		result := toolExecutionResult{}
		defer func() {
			if r := recover(); r != nil {
				result.panicValue = r
			}
			results <- result
		}()
		result.output, result.err = tool.Execute(ctx, args)
	}()

	select {
	case result := <-results:
		return result.output, result.err, result.panicValue
	case <-ctx.Done():
		return ToolOutput{}, ctx.Err(), nil
	}
}

// startToolHeartbeat emits heartbeat events only if a tool is still running
// after toolHeartbeatInterval. Most tools complete quickly, so using AfterFunc
// avoids starting a goroutine and ticker on every tool invocation.
func startToolHeartbeat(ctx context.Context, callID, toolName string, send eventSender) func() {
	if send.ch == nil {
		return func() {}
	}

	var stopped atomic.Bool
	heartbeat := Event{Type: EventHeartbeat, ToolCallID: callID, ToolName: toolName}

	timer := time.AfterFunc(toolHeartbeatInterval, func() {
		ticker := time.NewTicker(toolHeartbeatInterval)
		defer ticker.Stop()
		for {
			if stopped.Load() {
				return
			}
			send.TrySend(heartbeat)
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	})

	return func() {
		stopped.Store(true)
		timer.Stop()
	}
}

func toolErrorMessageWithGuardian(id, name, text string, thoughtSig []byte, reviews []GuardianReview) Message {
	message := ToolErrorMessage(id, name, text, thoughtSig)
	if len(message.Parts) > 0 && message.Parts[0].ToolResult != nil {
		message.Parts[0].ToolResult.GuardianReviews = append([]GuardianReview(nil), reviews...)
	}
	return message
}

const (
	reliableSpawnAgentTerminalName  = "spawn_agent"
	reliableWaitForJobsTerminalName = "wait_for_jobs"
)

// sendToolExecEnd keeps delegation completion lossless. A child session or
// queued run can become terminal before the parent tool result is durable, so
// dropping either event leaves Web clients with an in-flight placeholder and
// an unreleased response timer hold. Other tool ends remain best-effort to
// preserve the existing worker backpressure behavior.
func sendToolExecEnd(send eventSender, event Event) {
	switch event.ToolName {
	case reliableSpawnAgentTerminalName, reliableWaitForJobsTerminalName:
		_ = send.Send(event)
	default:
		send.TrySend(event)
	}
}

// executeSingleToolCall is the historical message adapter used by focused
// tests and non-batch callers.
func (e *Engine) executeSingleToolCall(ctx context.Context, call ToolCall, send eventSender, debug bool, debugRaw bool) ([]Message, error) {
	outcome := e.executeSingleToolCallOutcome(ctx, call, send, debug, debugRaw)
	return []Message{outcome.message()}, nil
}

// executeSingleToolCallOutcome executes a single tool call and retains its
// structured output for both transcript and bridge result adapters.
func (e *Engine) executeSingleToolCallOutcome(ctx context.Context, call ToolCall, send eventSender, debug bool, debugRaw bool) toolCallOutcome {
	tool, ok := e.tools.Get(call.Name)
	if !ok {
		// suggest_commands is a structured-output passthrough used by exec mode;
		// observing the call arguments is the execution, so no registry tool runs.
		if call.Name == SuggestCommandsToolName {
			output := TextOutput("OK")
			DebugToolResult(debug, call.ID, call.Name, output.Content)
			sendToolExecEnd(send, Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolSuccess: true})
			return toolCallOutcome{call: call, output: output}
		}
		errMsg := fmt.Sprintf("Error: tool not registered: %s", call.Name)
		DebugToolResult(debug, call.ID, call.Name, errMsg)
		sendToolExecEnd(send, Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolSuccess: false})
		return toolCallOutcome{call: call, err: fmt.Errorf("tool not registered: %s", call.Name)}
	}

	// Check ordinary execution policy first. A planner may separately authorise
	// only its exact control tool for this active run; this does not change the
	// public IsToolAllowed result or grant authority to any self-asserted tool.
	allowed := e.IsToolAllowed(call.Name)
	if !allowed {
		if planner := e.currentToolPlanner(); planner != nil {
			allowed = planner.AllowsPlannerTool(ToolRunIDFromContext(ctx), call.Name)
		}
	}
	if !allowed {
		errMsg := fmt.Sprintf("Error: tool '%s' is not in the active skill's allowed-tools list", call.Name)
		DebugToolResult(debug, call.ID, call.Name, errMsg)
		sendToolExecEnd(send, Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolSuccess: false})
		return toolCallOutcome{call: call, err: fmt.Errorf("tool '%s' is not in the active skill's allowed-tools list", call.Name)}
	}

	// Add call ID to context for spawn_agent event bubbling
	toolCtx := ContextWithCallID(ctx, call.ID)
	toolCtx, guardianReviews := ContextWithGuardianReviewCapture(toolCtx)

	stopHeartbeat := startToolHeartbeat(ctx, call.ID, call.Name, send)
	defer stopHeartbeat()

	output, err, panicValue := e.executeToolWithCancellation(toolCtx, tool, call.Arguments)
	output.GuardianReviews = guardianReviews()
	if planner := e.currentToolPlanner(); planner != nil {
		planner.ToolExecuted(SessionIDFromContext(ctx), ToolRunIDFromContext(ctx), call.Name)
	}
	if panicValue != nil {
		panic(panicValue)
	}
	info := e.getToolPreview(call)

	// Truncate large tool outputs (global limit, then compaction limit).
	if err == nil {
		output = e.applyToolOutputTruncation(output)
	}

	if err != nil {
		errMsg := fmt.Sprintf("Error: %v", err)
		DebugToolResult(debug, call.ID, call.Name, errMsg)
		sendToolExecEnd(send, Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: info, ToolSuccess: false})
		return toolCallOutcome{call: call, output: output, err: err}
	}

	DebugToolResult(debug, call.ID, call.Name, output.Content)
	DebugRawToolResult(debugRaw, call.ID, call.Name, output.Content)
	// Spawn-agent completion is reliable because its durable parent result may be
	// delayed behind other parallel tools. Other tool ends remain best-effort.
	sendToolExecEnd(send, Event{
		Type:                       EventToolExecEnd,
		ToolCallID:                 call.ID,
		ToolName:                   call.Name,
		ToolInfo:                   info,
		ToolSuccess:                !output.TimedOut && !output.IsError,
		ToolOutput:                 output.Content,
		ToolDiffs:                  output.Diffs,
		ToolFileChanges:            output.FileChanges,
		ToolFilesystemObservations: output.FilesystemObservations,
		ToolOutputClaimDiagnostics: output.OutputClaimDiagnostics,
		ToolImages:                 output.Images,
		ToolMedia:                  append([]MediaArtifact(nil), output.Media...),
	})
	return toolCallOutcome{call: call, output: output}
}

func (e *Engine) withToolPreview(calls []ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	withPreview := make([]ToolCall, len(calls))
	for i := range calls {
		withPreview[i] = calls[i]
		withPreview[i].ToolInfo = e.getToolPreview(withPreview[i])
	}
	return withPreview
}

func ensureToolCallIDs(calls []ToolCall) []ToolCall {
	for i := range calls {
		if strings.TrimSpace(calls[i].ID) == "" {
			calls[i].ID = newSyntheticToolCallID()
		}
	}
	return calls
}

func dedupeToolCalls(calls []ToolCall) []ToolCall {
	if len(calls) < 2 {
		return calls
	}
	seen := make(map[string]struct{}, len(calls))
	out := make([]ToolCall, 0, len(calls))
	for _, call := range calls {
		id := strings.TrimSpace(call.ID)
		if id == "" {
			out = append(out, call)
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, call)
	}
	return out
}

// getToolPreview returns a preview string for a tool call.
func (e *Engine) getToolPreview(call ToolCall) string {
	if call.ToolInfo != "" {
		return call.ToolInfo
	}
	if tool, ok := e.tools.Get(call.Name); ok {
		if preview := tool.Preview(call.Arguments); preview != "" {
			if !strings.HasPrefix(preview, "(") {
				return "(" + preview + ")"
			}
			return preview
		}
	}
	return ExtractToolInfo(call)
}

func formatToolArgs(args map[string]any, maxLen, maxParams int) string {
	if len(args) == 0 {
		return ""
	}

	type argPair struct {
		key string
		val string
	}
	var pairs []argPair

	for k, v := range args {
		var valStr string
		switch val := v.(type) {
		case string:
			if val == "" {
				continue
			}
			valStr = val
		case float64:
			if val == float64(int(val)) {
				valStr = fmt.Sprintf("%d", int(val))
			} else {
				valStr = fmt.Sprintf("%g", val)
			}
		case bool:
			valStr = fmt.Sprintf("%v", val)
		default:
			continue
		}

		if len(valStr) > 200 {
			valStr = valStr[:197] + "..."
		}
		pairs = append(pairs, argPair{key: k, val: valStr})
	}

	if len(pairs) == 0 {
		return ""
	}

	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].key < pairs[j].key
	})

	var result string
	if len(pairs) == 1 {
		result = "(" + pairs[0].val + ")"
	} else {
		var parts []string
		for i, p := range pairs {
			if i >= maxParams {
				parts = append(parts, "...")
				break
			}
			parts = append(parts, p.key+":"+p.val)
		}
		result = "(" + strings.Join(parts, ", ") + ")"
	}

	if len(result) > maxLen {
		result = result[:maxLen-4] + "...)"
	}

	return result
}

// ExtractToolInfo extracts a preview string from tool call arguments.
// Used for displaying tool calls in the UI (e.g., "(path:main.go)" for read_file).
func ExtractToolInfo(call ToolCall) string {
	if len(call.Arguments) == 0 {
		return ""
	}

	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return ""
	}

	return formatToolArgs(args, 500, 5)
}

// loggingStream wraps a stream to accumulate usage and log it on completion
type loggingStream struct {
	inner           Stream
	logger          *usage.Logger
	providerName    string
	model           string
	trackedExternal string // "claude-code", "codex", or "" for direct API

	// mu guards the accumulator/logged fields against concurrent Recv/Close.
	mu              sync.Mutex
	totalInput      int
	totalOutput     int
	totalCacheRead  int
	totalCacheWrite int
	logged          bool
}

func (s *loggingStream) Recv() (Event, error) {
	event, err := s.inner.Recv()

	if err == nil {
		switch event.Type {
		case EventUsage:
			if event.Use != nil {
				s.mu.Lock()
				s.totalInput += event.Use.InputTokens
				s.totalOutput += event.Use.OutputTokens
				s.totalCacheRead += event.Use.CachedInputTokens
				s.totalCacheWrite += event.Use.CacheWriteTokens
				s.mu.Unlock()
			}
		case EventDone:
			s.mu.Lock()
			if !s.logged {
				s.flushLocked()
			}
			s.mu.Unlock()
		}
		return event, nil
	}

	if err == io.EOF {
		s.mu.Lock()
		if !s.logged {
			s.flushLocked()
		}
		s.mu.Unlock()
	}

	return event, err
}

func (s *loggingStream) Close() error {
	s.mu.Lock()
	if !s.logged {
		s.flushLocked()
	}
	s.mu.Unlock()
	return s.inner.Close()
}

func (s *loggingStream) flushLocked() {
	if s.totalInput == 0 && s.totalOutput == 0 {
		return
	}
	s.logged = true
	_ = s.logger.Log(usage.LogEntry{
		Timestamp:           time.Now(),
		Model:               s.model,
		Provider:            s.providerName,
		InputTokens:         s.totalInput,
		OutputTokens:        s.totalOutput,
		CacheReadTokens:     s.totalCacheRead,
		CacheWriteTokens:    s.totalCacheWrite,
		TrackedExternallyBy: s.trackedExternal,
	})
}

// wrapLoggingStream wraps a stream with usage logging
func wrapLoggingStream(inner Stream, providerName, model string) Stream {
	// If model is empty, use providerName as the model identifier
	// This helps identify what was used when providers auto-select models
	if model == "" {
		model = providerName
	}
	return &loggingStream{
		inner:           inner,
		logger:          usage.DefaultLogger(),
		providerName:    providerName,
		model:           model,
		trackedExternal: usage.GetTrackedExternallyBy(providerName),
	}
}

// wrapDebugLoggingStream wraps a stream with debug logging if enabled
func (e *Engine) wrapDebugLoggingStream(inner Stream) Stream {
	if e.debugLogger == nil {
		return inner
	}
	return &debugLoggingStream{
		inner:  inner,
		logger: e.debugLogger,
	}
}

// debugLoggingStream wraps a stream to log events for debugging
type debugLoggingStream struct {
	inner  Stream
	logger *DebugLogger
}

func (s *debugLoggingStream) Recv() (Event, error) {
	event, err := s.inner.Recv()
	if err == nil {
		s.logger.LogEvent(event)
	}
	return event, err
}

func (s *debugLoggingStream) Close() error {
	return s.inner.Close()
}

// cleanupStream wraps a stream to call provider per-turn cleanup on terminal
// conditions (io.EOF, EventDone, or Close). Used for per-turn resources such
// as CLI-provider prompt/image files. MCP servers and other
// conversation-scoped state are cleaned up elsewhere (runtime eviction).
type cleanupStream struct {
	inner     Stream
	cleanup   func()
	closeOnce sync.Once
}

func (s *cleanupStream) Recv() (Event, error) {
	event, err := s.inner.Recv()
	// Trigger cleanup on terminal conditions (EOF or EventDone)
	// This ensures cleanup runs even if consumer doesn't call Close()
	if err == io.EOF || (err == nil && event.Type == EventDone) {
		if cleanupErr := s.cleanupOnce(); cleanupErr != nil {
			return Event{}, cleanupErr
		}
	}
	return event, err
}

func (s *cleanupStream) Close() error {
	err := s.inner.Close()
	if cleanupErr := s.cleanupOnce(); err == nil {
		err = cleanupErr
	}
	return err
}

func (s *cleanupStream) cleanupOnce() (err error) {
	if s.cleanup == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("stream cleanup panic: %v", r)
			}
		}()
		s.cleanup()
	})
	return err
}
