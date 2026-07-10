package llm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	"github.com/samsaffron/term-llm/internal/usage"
)

const (
	defaultMaxTurns                    = 50
	defaultMaxParallelToolCalls        = 20
	defaultUncommittedStreamMaxRetries = 5
	stopSearchToolHint                 = "IMPORTANT: Do not call any tools. Use the information already retrieved and answer directly."
	contextContinuationPrompt          = "Continue the task from the compacted context. Follow the pending next step; do not ask the user unless blocked."
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

type Engine struct {
	provider    Provider
	tools       *ToolRegistry
	debugLogger *DebugLogger

	// indirectVision routes user image parts through textual path references so
	// text-only models can call view_image instead of receiving image bytes.
	indirectVision atomic.Bool

	// allowedTools filters which tools can be executed.
	// If nil or empty, all tools are allowed. When set, only listed tools can run.
	// Used by skills with allowed-tools to restrict tool access.
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
	// onCompaction is called after context compaction completes.
	onCompaction CompactionCallback
	callbackMu   sync.RWMutex

	// Global tool output truncation
	maxToolOutputChars int // 0 = disabled; truncate tool output to this many runes

	// Context compaction
	compactionConfig     *CompactionConfig // nil = compaction disabled
	inputLimit           int               // 0 = unknown/disabled
	lastTotalTokens      int               // cached+input+output from most recent API response
	lastMessageCount     int               // retained/persisted for compatibility; estimator anchors structurally
	systemPrompt         string            // Captured for re-injection after compaction
	contextNoticeEmitted atomic.Bool       // one-shot flag: WARNING emitted once per session

	// Interjection support: users can send messages while the agent is streaming.
	// Messages are injected FIFO after the current turn's tool results, before
	// the next LLM turn. While entries remain in this queue they are cancellable;
	// draining atomically commits them for the next provider request.
	pendingInterjections []queuedInterjection

	// pendingRequestRuntime is a same-provider model/effort override requested
	// while an agentic loop is active. It is drained at the next provider-turn
	// boundary so UI effort changes can affect the next LLM call after tool
	// results without replacing the in-flight Engine.
	pendingRequestRuntime pendingRequestRuntimeSwitch

	// chaosFailNext is armed by TERM_LLM_CHAOS_MONKEY UI shortcuts to inject a
	// replayable stream failure at the next provider receive boundary.
	chaosFailNext atomic.Bool

	// pendingToolSpecs holds tool specs registered mid-loop (e.g. via skill activation)
	// that should be injected into req.Tools at the start of the next loop iteration.
	pendingToolSpecs []ToolSpec
	pendingToolsMu   sync.Mutex
}

// ToolExecutorSetter is an optional interface for providers that need
// tool execution wired up externally (for example, local CLI providers with HTTP MCP).
type ToolExecutorSetter interface {
	SetToolExecutor(func(ctx context.Context, name string, args json.RawMessage) (ToolOutput, error))
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

type InterjectionStatus string

const (
	InterjectionQueued    InterjectionStatus = "queued"
	InterjectionCommitted InterjectionStatus = "committed"
)

// QueuedInterjection is a structured user message submitted while a run is active.
// Queued entries are cancellable until the engine drains them into a provider turn.
type QueuedInterjection struct {
	ID           string
	Message      Message
	DisplayText  string
	Status       InterjectionStatus
	AutoContinue bool // If true, drain at a text-only turn boundary and continue the run.
}

type queuedInterjection = QueuedInterjection

var engineInterjectionID atomic.Uint64

func nextEngineInterjectionID() string {
	return fmt.Sprintf("interject_%d", engineInterjectionID.Add(1))
}

func NewEngine(provider Provider, tools *ToolRegistry) *Engine {
	if tools == nil {
		tools = NewToolRegistry()
	}
	e := &Engine{
		provider: provider,
		tools:    tools,
	}

	// Wire up tool executors for providers that expose term-llm tools over an external bridge.
	if setter, ok := provider.(ToolExecutorSetter); ok {
		setter.SetToolExecutor(func(ctx context.Context, name string, args json.RawMessage) (ToolOutput, error) {
			tool, ok := e.tools.Get(name)
			if !ok {
				return ToolOutput{}, fmt.Errorf("tool not found: %s", name)
			}
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

// AddDynamicTool registers a tool and queues its spec to be injected into
// the active agentic loop's tool list at the start of the next iteration.
// Use this instead of engine.Tools().Register() when activating skill tools
// mid-conversation so the LLM sees them immediately on the next turn.
func (e *Engine) AddDynamicTool(tool Tool) {
	e.tools.Register(tool)
	e.pendingToolsMu.Lock()
	e.pendingToolSpecs = append(e.pendingToolSpecs, tool.Spec())
	e.pendingToolsMu.Unlock()
}

// drainPendingToolSpecs returns any queued tool specs and clears the queue.
func (e *Engine) drainPendingToolSpecs() []ToolSpec {
	e.pendingToolsMu.Lock()
	defer e.pendingToolsMu.Unlock()
	if len(e.pendingToolSpecs) == 0 {
		return nil
	}
	specs := e.pendingToolSpecs
	e.pendingToolSpecs = nil
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
	e.contextNoticeEmitted.Store(false)
	e.callbackMu.Unlock()

	// Reset provider-side conversation state if supported
	resetProviderConversation(e.provider)
}

// SetDebugLogger sets the debug logger for this engine.
func (e *Engine) SetDebugLogger(logger *DebugLogger) {
	e.debugLogger = logger
}

// SetAllowedTools sets the list of tools that can be executed.
// When set, only tools in this list can run; all others are blocked.
// Pass nil or empty slice to allow all tools.
// The list is intersected with registered tools (can't allow unregistered tools).
func (e *Engine) SetAllowedTools(tools []string) {
	e.allowedMu.Lock()
	defer e.allowedMu.Unlock()

	if len(tools) == 0 {
		e.allowedTools = nil
		return
	}

	e.allowedTools = make(map[string]bool, len(tools))
	for _, name := range tools {
		// Only add if tool is registered (intersection with available tools)
		if _, ok := e.tools.Get(name); ok {
			e.allowedTools[name] = true
		}
	}
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
	var compactionConfig *CompactionConfig

	if provider != nil && !provider.Capabilities().ManagesOwnContext {
		limit = InputLimitForProviderModel(providerName, modelName)
		if limit == 0 {
			refreshDynamicModelLimitsForContext(provider, providerName, modelName)
			limit = InputLimitForProviderModel(providerName, modelName)
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
	if providerType != "copilot" || strings.TrimSpace(modelName) == "" {
		return
	}
	lister, ok := provider.(interface {
		ListModels(context.Context) ([]ModelInfo, error)
	})
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models, err := lister.ListModels(ctx)
	if err != nil {
		slog.Debug("failed to refresh Copilot model metadata for context limits", "provider", providerName, "model", modelName, "error", err)
		return
	}
	RefreshCopilotCacheSync(models)
}

// InputLimit returns the configured input token limit (0 if unknown).
func (e *Engine) InputLimit() int {
	e.callbackMu.RLock()
	v := e.inputLimit
	e.callbackMu.RUnlock()
	return v
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

// Interject queues a text user message to be inserted after the current turn's tool results,
// right before the next LLM turn begins. Safe to call from any goroutine.
func (e *Engine) Interject(text string) {
	e.InterjectWithID("", text)
}

// InterjectWithID behaves like Interject but preserves a caller-supplied stable
// identifier.
func (e *Engine) InterjectWithID(id, text string) {
	_ = e.QueueInterjection(QueuedInterjection{
		ID:          id,
		Message:     UserText(text),
		DisplayText: text,
	})
}

// QueueInterjection appends a structured interjection to the FIFO pending queue
// and returns its stable ID. The message role is normalized to RoleUser.
func (e *Engine) QueueInterjection(entry QueuedInterjection) string {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()

	if strings.TrimSpace(entry.ID) == "" {
		entry.ID = nextEngineInterjectionID()
	}
	entry.Message.Role = RoleUser
	if entry.DisplayText == "" {
		entry.DisplayText = MessageText(entry.Message)
		if strings.TrimSpace(entry.DisplayText) == "" {
			entry.DisplayText = MessageAttachmentSummary(entry.Message)
		}
	}
	entry.Status = InterjectionQueued
	e.pendingInterjections = append(e.pendingInterjections, entry)
	return entry.ID
}

// CancelInterjection removes a queued, not-yet-committed interjection. It returns
// false if the ID is unknown or has already been drained for a provider request.
func (e *Engine) CancelInterjection(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	for i := range e.pendingInterjections {
		if e.pendingInterjections[i].ID == id {
			copy(e.pendingInterjections[i:], e.pendingInterjections[i+1:])
			e.pendingInterjections[len(e.pendingInterjections)-1] = QueuedInterjection{}
			e.pendingInterjections = e.pendingInterjections[:len(e.pendingInterjections)-1]
			return true
		}
	}
	return false
}

// DiscardPendingInterjections removes all queued, not-yet-committed
// interjections and returns how many were discarded.
func (e *Engine) DiscardPendingInterjections() int {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	count := len(e.pendingInterjections)
	for i := range e.pendingInterjections {
		e.pendingInterjections[i] = QueuedInterjection{}
	}
	e.pendingInterjections = nil
	return count
}

// ListPendingInterjections returns a snapshot of queued, cancellable interjections.
func (e *Engine) ListPendingInterjections() []QueuedInterjection {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	out := make([]QueuedInterjection, len(e.pendingInterjections))
	copy(out, e.pendingInterjections)
	return out
}

// DrainInterjection returns pending interjection text and drains all queued
// interjections. It is retained for legacy recovery paths; new callers should
// use DrainInterjections or ListPendingInterjections.
func (e *Engine) DrainInterjection() string {
	entries := e.DrainInterjections()
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

// DrainInterjections drains all queued interjections and marks returned entries
// committed. Draining is the atomic handoff after which cancellation fails.
func (e *Engine) DrainInterjections() []QueuedInterjection {
	return e.drainInterjections()
}

// PeekInterjection returns a text summary of currently pending interjections.
func (e *Engine) PeekInterjection() string {
	entries := e.ListPendingInterjections()
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

// drainInterjections atomically commits all queued interjections.
func (e *Engine) drainInterjections() []queuedInterjection {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()

	if len(e.pendingInterjections) == 0 {
		return nil
	}
	out := make([]queuedInterjection, len(e.pendingInterjections))
	copy(out, e.pendingInterjections)
	for i := range out {
		out[i].Status = InterjectionCommitted
	}
	e.pendingInterjections = nil
	return out
}

// drainAutoContinueInterjections atomically commits the queued interjection
// prefix marked AutoContinue. It intentionally preserves FIFO order: a normal
// pending user interjection blocks later auto-continue notifications from being
// injected ahead of it.
func (e *Engine) drainAutoContinueInterjections() []queuedInterjection {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()

	if len(e.pendingInterjections) == 0 || !e.pendingInterjections[0].AutoContinue {
		return nil
	}
	n := 0
	for n < len(e.pendingInterjections) && e.pendingInterjections[n].AutoContinue {
		n++
	}
	out := make([]queuedInterjection, n)
	copy(out, e.pendingInterjections[:n])
	for i := range out {
		out[i].Status = InterjectionCommitted
	}
	copy(e.pendingInterjections, e.pendingInterjections[n:])
	for i := len(e.pendingInterjections) - n; i < len(e.pendingInterjections); i++ {
		e.pendingInterjections[i] = queuedInterjection{}
	}
	e.pendingInterjections = e.pendingInterjections[:len(e.pendingInterjections)-n]
	return out
}

func (e *Engine) continueWithAutoInterjections(ctx context.Context, send eventSender, req *Request, turnCallback TurnCompletedCallback, attempt int, finalMsg Message) (bool, error) {
	interjections := e.drainAutoContinueInterjections()
	if len(interjections) == 0 {
		return false, nil
	}
	if len(finalMsg.Parts) > 0 {
		req.Messages = append(req.Messages, finalMsg)
	}
	interjectionMsgs := make([]Message, 0, len(interjections))
	for _, interjection := range interjections {
		interjectionMsg := interjection.Message
		interjectionMsg.Role = RoleUser
		req.Messages = append(req.Messages, interjectionMsg)
		interjectionMsgs = append(interjectionMsgs, interjectionMsg)
	}
	if turnCallback != nil {
		cbCtx, cancel := callbackContext(ctx)
		_ = turnCallback(cbCtx, attempt, interjectionMsgs, TurnMetrics{})
		cancel()
	}
	for _, interjection := range interjections {
		text := interjection.DisplayText
		if text == "" {
			text = MessageText(interjection.Message)
		}
		if err := send.Send(Event{Type: EventInterjection, Text: text, InterjectionID: interjection.ID, Message: interjection.Message, InterjectionStatus: InterjectionCommitted}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// applyToolOutputTruncation applies global and compaction truncation limits
// to tool output content. Global limit fires first (typically stricter),
// then compaction limit as a secondary safety net.
func (e *Engine) applyToolOutputTruncation(content string) string {
	e.callbackMu.RLock()
	maxChars := e.maxToolOutputChars
	cc := e.compactionConfig
	e.callbackMu.RUnlock()

	if maxChars > 0 {
		content = TruncateToolResult(content, maxChars)
	}
	if cc != nil && cc.MaxToolResultChars > 0 {
		content = TruncateToolResult(content, cc.MaxToolResultChars)
	}
	return content
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

	// No filter means all tools are allowed
	if e.allowedTools == nil {
		return true
	}
	return e.allowedTools[name]
}

const indirectVisionInstruction = "Uploaded images are represented as local file-path references for this text-only model. When visual content matters, call the view_image tool with the referenced file_path and, if useful, a focused question. Do not claim to have inspected an image unless you have called view_image or the user-provided text is sufficient."

func (e *Engine) prepareProviderRequest(req Request) Request {
	if e.IndirectVision() {
		prepared := req
		normalized := ensureIndirectVisionImagePaths(req.Messages)
		rewritten, changed := rewriteImagePartsAsReferences(normalized)
		prepared.Messages = rewritten
		if changed {
			prepared.Messages = prependIndirectVisionInstruction(prepared.Messages)
		}
		return prepared
	}
	prepared := req
	prepared.Messages = hydrateImageDataFromPaths(req.Messages)
	return prepared
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

// Stream returns a stream, applying external tools when needed.
func (e *Engine) Stream(ctx context.Context, req Request) (Stream, error) {
	req.Messages = FilterConversationMessages(req.Messages)

	caps := e.provider.Capabilities()

	// 1. Handle external search/fetch tool injection
	// If Search is enabled, add web_search and read_url tools to the tool list.
	// The LLM will use them naturally during conversation like any other tool.
	if req.Search {
		needsExternalSearch := !caps.NativeWebSearch || req.ForceExternalSearch
		needsExternalFetch := (!caps.NativeWebFetch || req.ForceExternalSearch) && !req.DisableExternalWebFetch

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

	if req.DebugRaw {
		debugReq := e.prepareProviderRequest(req)
		DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), debugReq, "Request")
	}

	// 2. Decide if we use the agentic loop
	// We use it if request has tools AND provider supports tool calls
	useLoop := len(req.Tools) > 0 && caps.ToolCalls

	if useLoop {
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
	if e.debugLogger != nil {
		debugReq := e.prepareProviderRequest(req)
		e.debugLogger.LogRequest(e.provider.Name(), req.Model, debugReq)
	}
	stream := newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		return e.runSimpleScratchpad(ctx, req, send)
	})
	stream = wrapLoggingStream(stream, e.provider.Name(), req.Model)
	stream = e.wrapDebugLoggingStream(stream)
	return stream, nil
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
	inner                 Stream
	ctx                   context.Context
	mu                    sync.Mutex
	text                  *strings.Builder
	reasoning             *strings.Builder
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
		if event.Text != "" {
			s.reasoning.WriteString(event.Text)
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
	var priorErr error
	for retry := 0; ; retry++ {
		providerReq := e.prepareProviderRequest(req)
		stream, err := e.provider.Stream(ctx, providerReq)
		if err != nil {
			return err
		}

		var scratchpad []Event
		var textBuilder strings.Builder
		var reasoningBuilder strings.Builder
		var reasoningItemID string
		var reasoningEncryptedContent string
		var reasoningSummaryParts []string
		var reasoningKind ReasoningKind
		var metrics TurnMetrics
		var failed error

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
				scratchpad = append(scratchpad, event)
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
				}
			case EventReasoningDelta:
				if event.Text != "" {
					reasoningBuilder.WriteString(event.Text)
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
				scratchpad = append(scratchpad, event)
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
				scratchpad = append(scratchpad, event)
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
				}
			case EventImageGenerated:
				scratchpad = append(scratchpad, event)
				if err := send.Send(event); err != nil {
					_ = stream.Close()
					return err
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
			if len(scratchpad) > 0 {
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

		if textBuilder.Len() == 0 && reasoningBuilder.Len() == 0 && len(reasoningSummaryParts) == 0 && reasoningItemID == "" && reasoningEncryptedContent == "" && priorErr != nil {
			return priorErr
		}
		if turnCallback != nil && (textBuilder.Len() > 0 || reasoningBuilder.Len() > 0 || len(reasoningSummaryParts) > 0 || reasoningItemID != "" || reasoningEncryptedContent != "") {
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
			cbCtx, cancel := callbackContext(ctx)
			_ = turnCallback(cbCtx, 0, []Message{finalMsg}, metrics)
			cancel()
		}
		return send.Send(Event{Type: EventDone})
	}
}

func (e *Engine) runLoop(ctx context.Context, req Request, send eventSender) error {
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

	e.callbackMu.RLock()
	compactionConfig := e.compactionConfig
	inputLimit := e.inputLimit
	e.callbackMu.RUnlock()

	// Propagate provider-effective input limit into compaction config so
	// Compact() uses the correct limit instead of canonical model limits.
	if compactionConfig != nil && inputLimit > 0 {
		cc := *compactionConfig
		cc.InputLimit = inputLimit
		compactionConfig = &cc
	}

	// Capture system prompt for re-injection after compaction.
	// Use a local variable to avoid a data race with ResetConversation,
	// which writes e.systemPrompt="" under callbackMu on the UI goroutine.
	var systemPrompt string
	if inputLimit > 0 {
		for _, msg := range req.Messages {
			if msg.Role == RoleSystem {
				systemPrompt = collectTextParts(msg.Parts)
				break
			}
		}
	}

	var reactiveCompactionDone bool // prevents infinite retry if compacted context still overflows
	var recoveredToolWork bool      // tracks whether any tool-work recovery happened this run
	var recoveredToolCallIDs map[string]bool
	var recoveredAtMessageCount = -1
	var recoveryPriorErr error
	var uncommittedStreamRetries int // retries for failed provider attempts whose assistant output never crossed a commit boundary
	var uncommittedPriorErr error
	var softCheckpointInjected bool
	var softCheckpointInProgress bool
	var softCompactionUsage Usage
	var softCheckpointOriginalMessages []Message
	var softCheckpointPrepared preparedCompactionContext
	var softCheckpointOriginalCount int
	var resumeAfterCompaction bool
	softThresholdRatio := defaultSoftThresholdRatio
	hardThresholdRatio := defaultHardThresholdRatio
	if compactionConfig != nil {
		if compactionConfig.SoftThresholdRatio > 0 {
			softThresholdRatio = compactionConfig.SoftThresholdRatio
		} else if compactionConfig.ThresholdRatio > 0 {
			softThresholdRatio = compactionConfig.ThresholdRatio
		}
		if compactionConfig.HardThresholdRatio > 0 {
			hardThresholdRatio = compactionConfig.HardThresholdRatio
		} else if compactionConfig.ThresholdRatio > 0 {
			hardThresholdRatio = compactionConfig.ThresholdRatio
		}
	}
	contextThresholdState := func(messages []Message) (estimate, soft, hard int) {
		if compactionConfig == nil || inputLimit <= 0 {
			return 0, 0, 0
		}
		return e.estimatedTokens(messages), int(float64(inputLimit) * softThresholdRatio), int(float64(inputLimit) * hardThresholdRatio)
	}
	softCompactionThresholdReached := func(messages []Message) bool {
		est, soft, _ := contextThresholdState(messages)
		return soft > 0 && est >= soft
	}
	hardCompactionThresholdReached := func(messages []Message) bool {
		est, _, hard := contextThresholdState(messages)
		return hard > 0 && est >= hard
	}
	canCompactBeforeTurn := func(messages []Message) bool {
		// Do not compact a brand-new one-shot request. Once there is prior
		// conversation history (anything before the latest user/tool turn), the
		// first provider turn of a resumed/continued stream must be eligible too;
		// otherwise a large user follow-up can overflow before attempt > 0.
		nonSystem := nonSystemMessages(messages)
		return len(nonSystem) > 1
	}
	applyCompaction := func(result *CompactionResult) bool {
		if cb := e.getCompactionCallback(); cb != nil {
			if cbErr := cb(ctx, result); cbErr != nil {
				slog.Debug("compaction callback failed", "error", cbErr)
				return false
			}
		}
		// The compacted transcript replaces the conversation context. Clear any
		// provider-side server state (for example Responses previous_response_id) so
		// the next request sends the compacted summary instead of continuing from a
		// stale pre-compaction server transcript.
		resetProviderConversation(e.provider)
		req.Messages = result.NewMessages
		resumeAfterCompaction = true
		e.callbackMu.Lock()
		e.lastTotalTokens = 0
		e.lastMessageCount = 0
		e.callbackMu.Unlock()
		return true
	}
	resetSoftCheckpointState := func() {
		softCheckpointInProgress = false
		softCompactionUsage = Usage{}
		softCheckpointOriginalMessages = nil
		softCheckpointPrepared = preparedCompactionContext{}
		softCheckpointOriginalCount = 0
	}
	beginSoftCheckpoint := func() {
		softCheckpointInjected = true
		softCheckpointInProgress = true
		softCompactionUsage = Usage{}
		softCheckpointOriginalMessages = append([]Message(nil), req.Messages...)
		nonSystem := nonSystemMessages(softCheckpointOriginalMessages)
		softCheckpointPrepared = prepareCompactionContext(nonSystem, *compactionConfig, "")
		softCheckpointOriginalCount = len(nonSystem)
	}
	restoreAfterSoftCompactionFailure := func() {
		if len(req.Messages) > 0 && strings.TrimSpace(MessageText(req.Messages[len(req.Messages)-1])) == strings.TrimSpace(contextContinuationBriefPrompt) {
			req.Messages = req.Messages[:len(req.Messages)-1]
		}
		req.Tools = append([]ToolSpec(nil), originalTools...)
		req.ToolChoice = originalToolChoice
		resetProviderConversation(e.provider)
		resetSoftCheckpointState()
	}
	messagesWithoutTrailingBriefPrompt := func() []Message {
		messages := append([]Message(nil), req.Messages...)
		if len(messages) > 0 && strings.TrimSpace(MessageText(messages[len(messages)-1])) == strings.TrimSpace(contextContinuationBriefPrompt) {
			messages = messages[:len(messages)-1]
		}
		return messages
	}
	applySoftHardFallback := func() bool {
		if compactionConfig == nil {
			return false
		}
		fallbackMessages := softCheckpointOriginalMessages
		if len(fallbackMessages) == 0 {
			fallbackMessages = messagesWithoutTrailingBriefPrompt()
		}
		result, err := Compact(ctx, e.provider, req.Model, systemPrompt, nonSystemMessages(fallbackMessages), *compactionConfig)
		if err != nil {
			slog.Debug("soft compaction hard fallback failed", "error", err)
			return false
		}
		if !softCompactionUsage.IsZero() {
			result.Usage.Add(softCompactionUsage)
		}
		if !applyCompaction(result) {
			return false
		}
		resetSoftCheckpointState()
		req.Messages = append(req.Messages, UserText(contextContinuationPrompt))
		req.Tools = append([]ToolSpec(nil), originalTools...)
		req.ToolChoice = originalToolChoice
		return true
	}
	maybeCompactAfterLLMCall := func(pending []Message) bool {
		if compactionConfig == nil || !canCompactBeforeTurn(req.Messages) {
			return false
		}
		candidate := append(append([]Message(nil), req.Messages...), pending...)
		pendingHasToolCall := false
		for _, msg := range pending {
			for _, part := range msg.Parts {
				if part.ToolCall != nil {
					pendingHasToolCall = true
					break
				}
			}
			if pendingHasToolCall {
				break
			}
		}
		// Prefer compacting at clean end-of-turn boundaries (soft threshold). If
		// the LLM just returned tool calls, hold off until the hard threshold so we
		// don't unnecessarily compact while a tool call is loose. At the hard
		// threshold we must compact now, then replay the tool call/result into the
		// new compacted conversation.
		shouldCompact := softCompactionThresholdReached(candidate)
		if pendingHasToolCall {
			shouldCompact = hardCompactionThresholdReached(candidate)
		}
		if !shouldCompact {
			return false
		}
		if err := send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); err != nil {
			slog.Debug("send compaction phase failed", "error", err)
			return false
		}
		result, err := Compact(ctx, e.provider, req.Model, systemPrompt, nonSystemMessages(req.Messages), *compactionConfig)
		if err != nil {
			slog.Debug("post-response compaction failed", "error", err)
			return false
		}
		return applyCompaction(result)
	}
	applyPendingRequestModelSwitch := func(attempt int) error {
		pending := e.drainPendingRequestRuntimeSwitch()
		if pending.model == "" && pending.reasoningEffort == "" {
			return nil
		}
		targetModel := pending.model
		if targetModel == "" {
			targetModel = req.Model
		}
		targetEffort := pending.reasoningEffort
		if targetModel == req.Model && targetEffort == strings.TrimSpace(req.ReasoningEffort) {
			return nil
		}
		req.Model = targetModel
		req.ReasoningEffort = targetEffort
		if err := send.Send(Event{Type: EventModelSwitch, Text: targetModel, Model: targetModel, ReasoningEffort: targetEffort}); err != nil {
			return err
		}
		if req.DebugRaw {
			DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), req, fmt.Sprintf("Request model switched before turn %d", attempt))
		}
		return nil
	}
turnLoop:
	for attempt := 0; attempt < maxTurns; attempt++ {
		// Inject any tool specs registered mid-loop (e.g. via skill activation)
		if pending := e.drainPendingToolSpecs(); len(pending) > 0 {
			for _, spec := range pending {
				if !hasToolNamed(req.Tools, spec.Name) {
					req.Tools = append(req.Tools, spec)
				}
			}
		}

		// Pre-turn compaction check. This must also run on attempt 0 when the
		// request already contains prior conversation history (for example after a
		// user sends a new message in a long chat or after resuming a session). The
		// old attempt>0 guard skipped exactly that case and allowed oversized first
		// turns to hit the provider before auto-compaction had a chance to run.
		if compactionConfig != nil && canCompactBeforeTurn(req.Messages) {
			if hardCompactionThresholdReached(req.Messages) {
				if err := send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); err != nil {
					return err
				}
				result, err := Compact(ctx, e.provider, req.Model, systemPrompt, nonSystemMessages(req.Messages), *compactionConfig)
				if err == nil {
					applyCompaction(result)
				}
				// On error: continue with full context (best effort)
			} else if !softCheckpointInjected && len(req.Tools) > 0 && softCompactionThresholdReached(req.Messages) {
				if err := send.Send(Event{Type: EventPhase, Text: PhaseCompactingWriteBrief}); err != nil {
					return err
				}
				beginSoftCheckpoint()
				req.Messages = append(req.Messages, UserText(contextContinuationBriefPrompt))
				if e.provider.Capabilities().SupportsToolChoice {
					req.ToolChoice = ToolChoice{Mode: ToolChoiceNone}
				} else {
					req.Tools = nil
				}
			}
		}
		// Warning when compaction is disabled but tracking detects high usage
		if compactionConfig == nil && inputLimit > 0 && !e.contextNoticeEmitted.Load() && attempt > 0 {
			threshold := int(float64(inputLimit) * defaultThresholdRatio)
			est := e.estimatedTokens(req.Messages)
			if est >= threshold {
				e.contextNoticeEmitted.Store(true)
				pct := int(100 * float64(est) / float64(inputLimit))
				if err := send.Send(Event{Type: EventPhase, Text: fmt.Sprintf(WarningPhasePrefix+"context is %d%% full. Add auto_compact: true to your config to enable automatic compaction.", pct)}); err != nil {
					return err
				}
			}
		}
		// Prepare turn
		if attempt == maxTurns-1 && attempt > 0 {
			req.Messages = append(req.Messages, SystemText(stopSearchToolHint))
			if req.LastTurnToolChoice != nil {
				req.ToolChoice = *req.LastTurnToolChoice
			}
		} else if attempt > 0 && !softCheckpointInProgress {
			// Ensure we are in Auto mode for follow-up turns in the loop
			req.ToolChoice = ToolChoice{Mode: ToolChoiceAuto}
		}

		if resumeAfterCompaction && !softCheckpointInProgress {
			resumeAfterCompaction = false
			if err := send.Send(Event{Type: EventPhase, Text: PhaseCompactingResumeTask}); err != nil {
				return err
			}
		}

		if err := applyPendingRequestModelSwitch(attempt); err != nil {
			return err
		}

		providerReq := e.prepareProviderRequest(req)

		// Log per-turn request state
		// For attempt 0: captures state after applyExternalSearch modifications
		// For attempt > 0: captures tool results appended in previous turn
		if e.debugLogger != nil {
			e.debugLogger.LogTurnRequest(attempt, e.provider.Name(), providerReq.Model, providerReq)
		}

		if req.DebugRaw {
			DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), providerReq, fmt.Sprintf("Request (turn %d)", attempt))
		}

		stream, err := e.provider.Stream(ctx, providerReq)
		if err != nil {
			// Reactive compaction: if this is a context overflow error, try compacting and retrying (once)
			if compactionConfig != nil && isContextOverflowError(err) && !reactiveCompactionDone {
				reactiveCompactionDone = true
				if err := send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); err != nil {
					return err
				}
				if softCheckpointInProgress {
					if applySoftHardFallback() {
						attempt--
						continue
					}
				} else {
					result, compactErr := Compact(ctx, e.provider, req.Model, systemPrompt, nonSystemMessages(req.Messages), *compactionConfig)
					if compactErr == nil && applyCompaction(result) {
						attempt-- // Retry this turn
						continue
					}
				}
			}
			if softCheckpointInProgress {
				restoreAfterSoftCompactionFailure()
			}
			// Warn when compaction is disabled and we hit context overflow
			if compactionConfig == nil && inputLimit > 0 && !e.contextNoticeEmitted.Load() && isContextOverflowError(err) {
				e.contextNoticeEmitted.Store(true)
				if err := send.Send(Event{Type: EventPhase, Text: WarningPhasePrefix + "context overflow. Add auto_compact: true to your config to enable automatic compaction."}); err != nil {
					return err
				}
			}
			return err
		}

		// Collect tool calls and text, forward events, track metrics
		var toolCalls []ToolCall
		var textBuilder strings.Builder
		var reasoningBuilder strings.Builder // For reasoning summary/thinking content
		var reasoningItemID string
		var reasoningEncryptedContent string
		var reasoningSummaryParts []string
		var reasoningKind ReasoningKind
		var turnMetrics TurnMetrics
		var syncToolsExecuted bool     // Track if tools were executed via sync path (MCP)
		var finishingToolExecuted bool // Track if a finishing tool was executed (agent done)
		var syncToolCalls []ToolCall   // Track sync tool calls for message building
		var syncToolResults []Message  // Track sync tool results for message building
		var scratchpadEvents []Event   // Attempt-local visible model output that can be discarded/replayed until a tool boundary.
		scratchpadCommitted := false   // True after provider completion or after a tool-call boundary makes assistant work durable.
		stageOrSendModelEvent := func(event Event) error {
			if !scratchpadCommitted {
				scratchpadEvents = append(scratchpadEvents, event)
			}
			return send.Send(event)
		}
		flushScratchpad := func() error {
			scratchpadEvents = nil
			scratchpadCommitted = true
			return nil
		}
		// fireSnapshot invokes the AssistantSnapshotCallback with the currently
		// accumulated assistant state plus the supplied tool calls. Called before
		// each EventToolCall send so consumers persist "as we go" — content
		// survives process death between emission and tool execution.
		fireSnapshot := func(calls []ToolCall) {
			if snapshotCallback == nil {
				return
			}
			partial := ensureToolCallIDs(calls)
			partial = dedupeToolCalls(partial)
			msg := buildAssistantMessageWithReasoningMetadata(
				textBuilder.String(),
				e.withToolPreview(partial),
				reasoningBuilder.String(),
				reasoningSummaryParts,
				reasoningItemID,
				reasoningEncryptedContent,
				reasoningKind,
			)
			if len(msg.Parts) == 0 {
				return
			}
			cbCtx, cancel := callbackContext(ctx)
			_ = snapshotCallback(cbCtx, attempt, msg)
			cancel()
		}
		// recoverCommittedToolWork journals completed tool-call requests and their
		// results, then continues the agent loop from the updated transcript. This
		// mirrors Codex's recovery model: if a stream dies after the model has asked
		// for a side-effecting tool, do not abandon the turn and do not retry from the
		// stale original prompt. Record the assistant tool call, execute/drain the
		// tool result, append both to req.Messages, and let the next loop iteration
		// continue from that journaled state.
		recoveryCompleted := false
		recoverCommittedToolWork := func(cause error) (bool, error) {
			if !isCommittedStreamRecoveryError(cause) {
				return false, nil
			}
			if len(toolCalls) == 0 && !syncToolsExecuted {
				return false, nil
			}

			if len(toolCalls) > 0 {
				candidate := ensureToolCallIDs(dedupeToolCalls(toolCalls))
				allRecovered := len(candidate) > 0 && recoveredToolCallIDs != nil
				for _, call := range candidate {
					if !recoveredToolCallIDs[call.ID] {
						allRecovered = false
						break
					}
				}
				if allRecovered {
					return false, nil
				}
			}

			if err := send.Send(Event{Type: EventRetry, RetryAttempt: 1, RetryMaxAttempts: 1, RetryWaitSecs: 0}); err != nil {
				return false, err
			}
			if cause != nil {
				slog.Debug("recovering stream failure from journaled tool work", "error", cause)
			}
			recoveredToolWork = true
			recoveredAtMessageCount = len(req.Messages)
			recoveryPriorErr = cause

			if len(toolCalls) == 0 && syncToolsExecuted {
				assistantMsg := buildAssistantMessageWithReasoningMetadata(
					textBuilder.String(),
					e.withToolPreview(syncToolCalls),
					reasoningBuilder.String(),
					reasoningSummaryParts,
					reasoningItemID,
					reasoningEncryptedContent,
					reasoningKind,
				)
				maybeCompactAfterLLMCall(append([]Message{assistantMsg}, syncToolResults...))
				req.Messages = append(req.Messages, assistantMsg)
				req.Messages = append(req.Messages, syncToolResults...)
				if err := applyPendingRequestModelSwitch(attempt + 1); err != nil {
					return false, err
				}
				recoveredAtMessageCount = len(req.Messages)
				if turnCallback != nil {
					turnMetrics.ToolCalls = len(syncToolCalls)
					turnMessages := []Message{assistantMsg}
					turnMessages = append(turnMessages, syncToolResults...)
					cbCtx, cancel := callbackContext(ctx)
					_ = turnCallback(cbCtx, attempt, turnMessages, turnMetrics)
					cancel()
				}
				return true, nil
			}

			toolCalls = ensureToolCallIDs(toolCalls)
			toolCalls = dedupeToolCalls(toolCalls)
			if recoveredToolCallIDs == nil {
				recoveredToolCallIDs = make(map[string]bool)
			}
			for _, call := range toolCalls {
				recoveredToolCallIDs[call.ID] = true
			}

			var registered, unregistered []ToolCall
			for _, call := range toolCalls {
				lookupName := call.Name
				if req.ToolMap != nil {
					if mapped, ok := req.ToolMap[call.Name]; ok {
						lookupName = mapped
					}
				}
				if _, ok := e.tools.Get(lookupName); ok {
					registered = append(registered, call)
				} else {
					unregistered = append(unregistered, call)
				}
			}

			if len(registered) == 0 {
				// Nothing local can be executed, but the assistant tool-call item is still
				// a completed model item. Journal it so a caller/session can resume with
				// the exact model-visible state that existed at the disconnect.
				unregisteredWithInfo := e.withToolPreview(unregistered)
				assistantMsg := buildAssistantMessageWithReasoningMetadata(
					textBuilder.String(),
					unregisteredWithInfo,
					reasoningBuilder.String(),
					reasoningSummaryParts,
					reasoningItemID,
					reasoningEncryptedContent,
					reasoningKind,
				)
				if len(assistantMsg.Parts) > 0 {
					maybeCompactAfterLLMCall([]Message{assistantMsg})
					req.Messages = append(req.Messages, assistantMsg)
					recoveredAtMessageCount = len(req.Messages)
					if turnCallback != nil {
						cbCtx, cancel := callbackContext(ctx)
						_ = turnCallback(cbCtx, attempt, []Message{assistantMsg}, turnMetrics)
						cancel()
					}
				}
				return false, cause
			}

			assistantMsg := buildAssistantMessageWithReasoningMetadata(
				textBuilder.String(),
				e.withToolPreview(registered),
				reasoningBuilder.String(),
				reasoningSummaryParts,
				reasoningItemID,
				reasoningEncryptedContent,
				reasoningKind,
			)
			maybeCompactAfterLLMCall([]Message{assistantMsg})
			responseHandled := callResponseCompletedCallback(ctx, responseCallback, attempt, assistantMsg, turnMetrics)

			var origNameByID map[string]string
			if req.ToolMap != nil {
				for i := range registered {
					if mapped, ok := req.ToolMap[registered[i].Name]; ok {
						if origNameByID == nil {
							origNameByID = make(map[string]string)
						}
						origNameByID[registered[i].ID] = registered[i].Name
						registered[i].Name = mapped
					}
				}
			}

			for _, call := range registered {
				DebugToolCall(req.Debug, call)
				info := e.getToolPreview(call)
				if err := send.Send(Event{Type: EventToolExecStart, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: info, ToolArgs: call.Arguments}); err != nil {
					return false, err
				}
			}

			transcriptForApproval := append(append([]Message(nil), req.ApprovalTranscriptPrefix...), req.Messages...)
			transcriptForApproval = append(transcriptForApproval, assistantMsg)
			toolResults, err := e.executeToolCalls(ctx, registered, req.ParallelToolCalls, send, req.Debug, req.DebugRaw, transcriptForApproval)
			if err != nil {
				return false, err
			}

			finishingToolExecuted = false
			for _, call := range registered {
				if e.tools.IsFinishingTool(call.Name) {
					finishingToolExecuted = true
					break
				}
			}

			if origNameByID != nil {
				for i := range registered {
					if orig, ok := origNameByID[registered[i].ID]; ok {
						registered[i].Name = orig
					}
				}
				for i := range toolResults {
					for j := range toolResults[i].Parts {
						if toolResults[i].Parts[j].ToolResult != nil {
							if orig, ok := origNameByID[toolResults[i].Parts[j].ToolResult.ID]; ok {
								toolResults[i].Parts[j].ToolResult.Name = orig
							}
						}
					}
				}
			}

			req.Messages = append(req.Messages, assistantMsg)
			req.Messages = append(req.Messages, toolResults...)
			if err := applyPendingRequestModelSwitch(attempt + 1); err != nil {
				return false, err
			}
			recoveredAtMessageCount = len(req.Messages)
			if turnCallback != nil {
				turnMetrics.ToolCalls = len(registered)
				turnMessages := turnMessagesAfterResponseCallback(responseHandled, assistantMsg, toolResults)
				cbCtx, cancel := callbackContext(ctx)
				_ = turnCallback(cbCtx, attempt, turnMessages, turnMetrics)
				cancel()
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if finishingToolExecuted {
				if err := send.Send(Event{Type: EventDone}); err != nil {
					return false, err
				}
				recoveryCompleted = true
				return true, nil
			}
			return true, nil
		}
		retryUncommittedAttempt := func(cause error) (bool, error) {
			if cause == nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || !isUncommittedReplayableStreamError(cause) {
				return false, nil
			}
			if recoveredToolWork || len(toolCalls) > 0 || syncToolsExecuted || scratchpadCommitted {
				return false, nil
			}
			if uncommittedStreamRetries >= defaultUncommittedStreamMaxRetries {
				return false, nil
			}
			uncommittedStreamRetries++
			uncommittedPriorErr = cause
			if len(scratchpadEvents) > 0 {
				if err := send.Send(Event{Type: EventAttemptDiscard}); err != nil {
					return false, err
				}
			}
			if err := send.Send(Event{
				Type:             EventRetry,
				RetryAttempt:     uncommittedStreamRetries,
				RetryMaxAttempts: defaultUncommittedStreamMaxRetries,
				RetryWaitSecs:    0,
			}); err != nil {
				return false, err
			}
			slog.Debug("retrying failed uncommitted model stream", "attempt", uncommittedStreamRetries, "error", cause)
			// Drop all attempt-local assistant output. Nothing from this provider
			// attempt crossed a durable boundary, so replaying the same request is safe.
			scratchpadEvents = nil
			if softCheckpointInProgress {
				softCompactionUsage = Usage{}
			}
			return true, nil
		}
		for {
			if chaosErr := e.consumeChaosFailure(); chaosErr != nil {
				stream.Close()
				if recoveredToolWork && len(req.Messages) == recoveredAtMessageCount && len(toolCalls) == 0 && !syncToolsExecuted {
					return chaosErr
				}
				if retried, retryErr := retryUncommittedAttempt(chaosErr); retryErr != nil {
					return retryErr
				} else if retried {
					attempt--
					continue turnLoop
				}
				if recovered, recoverErr := recoverCommittedToolWork(chaosErr); recoverErr != nil {
					return recoverErr
				} else if recoveryCompleted {
					return nil
				} else if recovered {
					continue turnLoop
				}
				return chaosErr
			}
			event, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				stream.Close()
				if compactionConfig != nil && isContextOverflowError(err) && !reactiveCompactionDone && textBuilder.Len() == 0 && reasoningBuilder.Len() == 0 && len(toolCalls) == 0 && len(syncToolCalls) == 0 {
					reactiveCompactionDone = true
					if sendErr := send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); sendErr != nil {
						return sendErr
					}
					if softCheckpointInProgress {
						if applySoftHardFallback() {
							attempt--
							continue turnLoop
						}
					} else {
						result, compactErr := Compact(ctx, e.provider, req.Model, systemPrompt, nonSystemMessages(req.Messages), *compactionConfig)
						if compactErr == nil && applyCompaction(result) {
							attempt-- // Retry this turn
							continue turnLoop
						}
					}
				}
				if recoveredToolWork && len(req.Messages) == recoveredAtMessageCount && len(toolCalls) == 0 && !syncToolsExecuted {
					return err
				}
				if retried, retryErr := retryUncommittedAttempt(err); retryErr != nil {
					return retryErr
				} else if retried {
					attempt--
					continue turnLoop
				}
				if recovered, recoverErr := recoverCommittedToolWork(err); recoverErr != nil {
					return recoverErr
				} else if recoveryCompleted {
					return nil
				} else if recovered {
					continue turnLoop
				}
				if softCheckpointInProgress {
					restoreAfterSoftCompactionFailure()
				}
				return err
			}
			if event.Type == EventError && event.Err != nil {
				stream.Close()
				if compactionConfig != nil && isContextOverflowError(event.Err) && !reactiveCompactionDone && textBuilder.Len() == 0 && reasoningBuilder.Len() == 0 && len(toolCalls) == 0 && len(syncToolCalls) == 0 {
					reactiveCompactionDone = true
					if sendErr := send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); sendErr != nil {
						return sendErr
					}
					if softCheckpointInProgress {
						if applySoftHardFallback() {
							attempt--
							continue turnLoop
						}
					} else {
						result, compactErr := Compact(ctx, e.provider, req.Model, systemPrompt, nonSystemMessages(req.Messages), *compactionConfig)
						if compactErr == nil && applyCompaction(result) {
							attempt-- // Retry this turn
							continue turnLoop
						}
					}
				}
				if recoveredToolWork && len(req.Messages) == recoveredAtMessageCount && len(toolCalls) == 0 && !syncToolsExecuted {
					return event.Err
				}
				if retried, retryErr := retryUncommittedAttempt(event.Err); retryErr != nil {
					return retryErr
				} else if retried {
					attempt--
					continue turnLoop
				}
				if recovered, recoverErr := recoverCommittedToolWork(event.Err); recoverErr != nil {
					return recoverErr
				} else if recoveryCompleted {
					return nil
				} else if recovered {
					continue turnLoop
				}
				if softCheckpointInProgress {
					restoreAfterSoftCompactionFailure()
				}
				return event.Err
			}
			if req.DebugRaw {
				DebugRawEvent(true, event)
			}
			if event.Type == EventAttemptDiscard {
				textBuilder.Reset()
				reasoningBuilder.Reset()
				reasoningItemID = ""
				reasoningEncryptedContent = ""
				reasoningSummaryParts = nil
				reasoningKind = ""
				turnMetrics = TurnMetrics{}
				if softCheckpointInProgress {
					softCompactionUsage = Usage{}
					continue
				}
				if err := stageOrSendModelEvent(event); err != nil {
					return err
				}
				continue
			}
			// Track usage metrics
			if event.Type == EventUsage && event.Use != nil {
				if softCheckpointInProgress {
					softCompactionUsage.Add(*event.Use)
				} else {
					turnMetrics.InputTokens += event.Use.InputTokens
					turnMetrics.OutputTokens += event.Use.OutputTokens
					turnMetrics.CachedInputTokens += event.Use.CachedInputTokens
					turnMetrics.CacheWriteTokens += event.Use.CacheWriteTokens
				}
				// Update token tracking for compaction threshold and status line display.
				// InputTokens is the non-cached portion; CachedInputTokens is the cached
				// portion. Together they equal the total context size this turn. Adding
				// OutputTokens gives the baseline for the next turn's input estimate
				// (the model's output becomes assistant-message input on the next turn).
				// All providers normalise to this convention — see Usage type docs.
				if inputLimit > 0 {
					messageCount := len(req.Messages)
					// Usage total includes the assistant output from this provider turn.
					// That output becomes assistant-message input on the next request, so
					// retain a compatibility count that includes the in-flight assistant
					// row when this usage event carries provider output.
					if event.Use.OutputTokens > 0 || textBuilder.Len() > 0 || reasoningBuilder.Len() > 0 || len(toolCalls) > 0 || len(syncToolCalls) > 0 || reasoningItemID != "" || reasoningEncryptedContent != "" {
						messageCount++
					}
					e.callbackMu.Lock()
					e.lastTotalTokens = event.Use.InputTokens + event.Use.CachedInputTokens + event.Use.OutputTokens
					e.lastMessageCount = messageCount
					e.callbackMu.Unlock()
				}
				if softCheckpointInProgress {
					continue
				}
				if err := stageOrSendModelEvent(event); err != nil {
					return err
				}
				continue
			}
			// Accumulate text for callback
			if event.Type == EventTextDelta && event.Text != "" {
				textBuilder.WriteString(event.Text)
				if softCheckpointInProgress {
					continue
				}
				if err := stageOrSendModelEvent(event); err != nil {
					return err
				}
				continue
			}
			// Accumulate reasoning for thinking models (OpenRouter)
			if event.Type == EventReasoningDelta && event.Text != "" {
				reasoningBuilder.WriteString(event.Text)
				reasoningKind = MergeReasoningKind(reasoningKind, event.ReasoningKind)
			}
			if event.Type == EventReasoningDelta {
				if len(event.ReasoningSummaryParts) > 0 {
					reasoningSummaryParts = append([]string(nil), event.ReasoningSummaryParts...)
					reasoningKind = MergeReasoningKind(reasoningKind, ReasoningKindSummary)
				}
				if event.ReasoningItemID != "" {
					reasoningItemID = event.ReasoningItemID
				}
				if event.ReasoningEncryptedContent != "" {
					reasoningEncryptedContent = event.ReasoningEncryptedContent
					reasoningKind = MergeReasoningKind(reasoningKind, event.ReasoningKind)
				}
				if softCheckpointInProgress {
					continue
				}
				if err := stageOrSendModelEvent(event); err != nil {
					return err
				}
				continue
			}
			if event.Type == EventToolCall && softCheckpointInProgress {
				// The continuation-brief turn is internal and explicitly forbids tools.
				// If a provider ignores tool_choice=none, discard the tool call and let
				// the no-brief fallback compact hard at the end of the stream.
				continue
			}
			if event.Type == EventToolCall && event.Tool != nil {
				if err := flushScratchpad(); err != nil {
					return err
				}
				// Check if this is a synchronous tool execution request from a provider bridge.
				if event.ToolResponse != nil {
					// Normalize Tool.ID from ToolCallID if the provider didn't set it.
					if event.Tool.ID == "" && event.ToolCallID != "" {
						event.Tool.ID = event.ToolCallID
					}
					// Forward the EventToolCall so consumers can see tool calls (e.g., exec.go needs
					// to see suggest_commands calls to parse suggestions from the arguments).
					// Create a copy without ToolResponse to avoid confusion.
					forwardEvent := Event{
						Type:       EventToolCall,
						ToolCallID: event.ToolCallID,
						ToolName:   event.ToolName,
						Tool:       event.Tool,
					}
					fireSnapshot(append(append([]ToolCall(nil), syncToolCalls...), *event.Tool))
					if err := send.Send(forwardEvent); err != nil {
						return err
					}

					// Handle synchronous execution: emit events to TUI and send result back
					call, result, execErr := e.handleSyncToolExecution(ctx, event, send, req.Debug, req.DebugRaw)
					syncToolsExecuted = true
					syncToolCalls = append(syncToolCalls, call)
					// Build result message for this tool call
					if execErr != nil {
						syncToolResults = append(syncToolResults, ToolErrorMessage(call.ID, call.Name, execErr.Error(), nil))
					} else {
						syncToolResults = append(syncToolResults, ToolResultMessageFromOutput(call.ID, call.Name, result, nil))
					}
					// Check if this was a finishing tool (signals agent completion)
					if e.tools.IsFinishingTool(event.Tool.Name) {
						finishingToolExecuted = true
					}
					continue
				}
				// Normal async collection for other providers.
				// Resolve a canonical tool call ID: prefer event.ToolCallID, fall back
				// to Tool.ID, and generate a stable synthetic ID if both are empty.
				toolCallID := event.ToolCallID
				if toolCallID == "" {
					toolCallID = event.Tool.ID
				}
				if toolCallID == "" {
					toolCallID = fmt.Sprintf("stream-toolcall-%d", len(toolCalls)+1)
				}
				// Normalize: ensure Tool.ID is always populated so downstream
				// consumers (serve handlers, API responses) get the correct ID.
				if event.Tool.ID == "" {
					event.Tool.ID = toolCallID
				}

				info := event.ToolInfo
				if info == "" {
					info = event.Tool.ToolInfo
				}
				if info == "" {
					info = e.getToolPreview(*event.Tool)
				}
				event.Tool.ToolInfo = info
				toolCalls = append(toolCalls, *event.Tool)

				fireSnapshot(toolCalls)
				if err := send.Send(Event{
					Type:       EventToolCall,
					ToolCallID: toolCallID,
					ToolName:   event.Tool.Name,
					Tool:       event.Tool,
					ToolInfo:   info,
				}); err != nil {
					return err
				}
				continue
			}
			if event.Type == EventImageGenerated {
				if err := stageOrSendModelEvent(event); err != nil {
					return err
				}
				continue
			}
			if event.Type == EventDone {
				continue
			}
			if err := send.Send(event); err != nil {
				return err
			}
		}
		stream.Close()

		// Exit promptly if caller cancelled while we were streaming.
		if err := ctx.Err(); err != nil {
			return err
		}

		// The stream reached its provider-defined end without an error. Commit any
		// attempt-local assistant output before callbacks and final done/tool handling.
		if err := flushScratchpad(); err != nil {
			return err
		}

		// Search is only performed once (either pre-emptively or in first turn)
		req.Search = false

		if len(toolCalls) == 0 && !syncToolsExecuted {
			if recoveredToolWork && textBuilder.Len() == 0 && reasoningBuilder.Len() == 0 && reasoningItemID == "" && reasoningEncryptedContent == "" && recoveryPriorErr != nil {
				return recoveryPriorErr
			}
			if uncommittedPriorErr != nil && textBuilder.Len() == 0 && reasoningBuilder.Len() == 0 && reasoningItemID == "" && reasoningEncryptedContent == "" {
				return uncommittedPriorErr
			}
			// No tools called - check if we should restore original tool choice and retry once
			if originalToolChoice.Mode == ToolChoiceName && !restoredToolChoice && !softCheckpointInjected {
				req.ToolChoice = originalToolChoice
				restoredToolChoice = true
				continue
			}
			// Call turnCallback with final text-only response (no tools)
			// Note: responseCallback is NOT called here because no tool execution follows.
			// responseCallback is only for persisting assistant messages before tool execution.
			var finalMsg Message
			if textBuilder.Len() > 0 || reasoningBuilder.Len() > 0 || len(reasoningSummaryParts) > 0 || reasoningItemID != "" || reasoningEncryptedContent != "" {
				finalMsg = buildAssistantMessageWithReasoningMetadata(
					textBuilder.String(),
					nil,
					reasoningBuilder.String(),
					reasoningSummaryParts,
					reasoningItemID,
					reasoningEncryptedContent,
					reasoningKind,
				)
				if softCheckpointInProgress {
					brief := continuationBriefFromAssistantMessage(finalMsg)
					if brief != "" && compactionConfig != nil && softCheckpointOriginalCount > 0 {
						result := compactionResultFromBriefPrepared(systemPrompt, brief, softCheckpointPrepared, softCheckpointOriginalCount, *compactionConfig)
						result.Usage = softCompactionUsage
						if applyCompaction(result) {
							resetSoftCheckpointState()
							req.Messages = append(req.Messages, UserText(contextContinuationPrompt))
							req.Tools = append([]ToolSpec(nil), originalTools...)
							req.ToolChoice = originalToolChoice
							attempt-- // brief/compaction is internal work; do not consume a normal agent turn
							continue
						}
					} else if compactionConfig != nil {
						if err := send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); err != nil {
							return err
						}
						if applySoftHardFallback() {
							attempt--
							continue
						}
					}
					restoreAfterSoftCompactionFailure()
					attempt-- // retry the task without treating the internal brief as a user-visible response
					continue
				}
				maybeCompactAfterLLMCall([]Message{finalMsg})
				if turnCallback != nil {
					cbCtx, cancel := callbackContext(ctx)
					_ = turnCallback(cbCtx, attempt, []Message{finalMsg}, turnMetrics)
					cancel()
				}
			}
			if softCheckpointInProgress {
				if compactionConfig != nil {
					if err := send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); err != nil {
						return err
					}
					if applySoftHardFallback() {
						attempt--
						continue
					}
				}
				restoreAfterSoftCompactionFailure()
				attempt--
				continue
			}
			// Auto-continue interjections are completion notices from background work.
			// Unlike ordinary user interjections during prose, they should be handed to
			// the provider immediately so the active web run can acknowledge them rather
			// than leaving the session stopped with a pending notice.
			if attempt < maxTurns-1 {
				continued, err := e.continueWithAutoInterjections(ctx, send, &req, turnCallback, attempt, finalMsg)
				if err != nil {
					return err
				}
				if continued {
					if err := applyPendingRequestModelSwitch(attempt + 1); err != nil {
						return err
					}
					continue
				}
			}
			if err := send.Send(Event{Type: EventDone}); err != nil {
				return err
			}
			return nil
		}

		// If only sync tools were executed (MCP path), decide whether to continue
		if len(toolCalls) == 0 && syncToolsExecuted {
			// Build assistant message with text and sync tool calls
			// This persists bridged CLI tool context for resume/rehydration.
			assistantMsg := buildAssistantMessageWithReasoningMetadata(
				textBuilder.String(),
				e.withToolPreview(syncToolCalls),
				reasoningBuilder.String(),
				reasoningSummaryParts,
				reasoningItemID,
				reasoningEncryptedContent,
				reasoningKind,
			)
			maybeCompactAfterLLMCall(append([]Message{assistantMsg}, syncToolResults...))
			req.Messages = append(req.Messages, assistantMsg)
			req.Messages = append(req.Messages, syncToolResults...)
			if !e.provider.Capabilities().InlineToolLoop {
				if err := applyPendingRequestModelSwitch(attempt + 1); err != nil {
					return err
				}
			}

			// For MCP path, tools already executed synchronously during streaming,
			// so we call turnCallback with the complete turn (assistant + tool results).
			// ResponseCallback was effectively the streaming itself.
			if turnCallback != nil {
				turnMetrics.ToolCalls = len(syncToolCalls)
				turnMessages := []Message{assistantMsg}
				turnMessages = append(turnMessages, syncToolResults...)
				cbCtx, cancel := callbackContext(ctx)
				_ = turnCallback(cbCtx, attempt, turnMessages, turnMetrics)
				cancel()
			}

			// Check for user interjections (MCP sync path)
			if interjections := e.drainInterjections(); len(interjections) > 0 {
				interjectionMsgs := make([]Message, 0, len(interjections))
				for _, interjection := range interjections {
					interjectionMsg := interjection.Message
					interjectionMsg.Role = RoleUser
					req.Messages = append(req.Messages, interjectionMsg)
					interjectionMsgs = append(interjectionMsgs, interjectionMsg)
				}
				if turnCallback != nil {
					cbCtx, cancel := callbackContext(ctx)
					_ = turnCallback(cbCtx, attempt, interjectionMsgs, TurnMetrics{})
					cancel()
				}
				for _, interjection := range interjections {
					text := interjection.DisplayText
					if text == "" {
						text = MessageText(interjection.Message)
					}
					if err := send.Send(Event{Type: EventInterjection, Text: text, InterjectionID: interjection.ID, Message: interjection.Message, InterjectionStatus: InterjectionCommitted}); err != nil {
						return err
					}
				}
				if !e.provider.Capabilities().InlineToolLoop {
					if err := applyPendingRequestModelSwitch(attempt + 1); err != nil {
						return err
					}
				}
			}

			// If a finishing tool was executed, we're done (agent completed its task)
			if finishingToolExecuted {
				if err := send.Send(Event{Type: EventDone}); err != nil {
					return err
				}
				return nil
			}

			// Inline-loop providers have already consumed tool results and streamed
			// their final answer in this invocation. Persisted interjections are
			// intentionally delivered on the next user turn.
			if e.provider.Capabilities().InlineToolLoop {
				if err := send.Send(Event{Type: EventDone}); err != nil {
					return err
				}
				return nil
			}

			// Continue the loop - provider will receive updated messages on next turn
			continue
		}

		toolCalls = ensureToolCallIDs(toolCalls)
		toolCalls = dedupeToolCalls(toolCalls)

		// Split into registered (to execute) and unregistered (to passthrough).
		// ToolMap allows client tool names to be redirected to server tools
		// (e.g. "WebSearch" → "search"). The call keeps its original name
		// so the client sees the name it expects in the response.
		var registered, unregistered []ToolCall
		for _, call := range toolCalls {
			lookupName := call.Name
			if req.ToolMap != nil {
				if mapped, ok := req.ToolMap[call.Name]; ok {
					lookupName = mapped
				}
			}
			if _, ok := e.tools.Get(lookupName); ok {
				registered = append(registered, call)
			} else {
				unregistered = append(unregistered, call)
			}
		}

		// Debug log unregistered tool calls (already forwarded during streaming)
		for i := range unregistered {
			DebugToolCall(req.Debug, unregistered[i])
		}

		// If nothing to execute, we are done
		if len(registered) == 0 {
			// Call turnCallback with text + unregistered tool calls
			// Note: responseCallback is NOT called here because no tool execution follows.
			// responseCallback is only for persisting assistant messages before tool execution.
			unregisteredWithInfo := e.withToolPreview(unregistered)
			finalMsg := buildAssistantMessageWithReasoningMetadata(
				textBuilder.String(),
				unregisteredWithInfo,
				reasoningBuilder.String(),
				reasoningSummaryParts,
				reasoningItemID,
				reasoningEncryptedContent,
				reasoningKind,
			)
			if len(finalMsg.Parts) > 0 {
				maybeCompactAfterLLMCall([]Message{finalMsg})
				if turnCallback != nil {
					cbCtx, cancel := callbackContext(ctx)
					_ = turnCallback(cbCtx, attempt, []Message{finalMsg}, turnMetrics)
					cancel()
				}
			}
			// Auto-continue interjections are completion notices from background work.
			// Unlike ordinary user interjections during prose, they should be handed to
			// the provider immediately so the active web run can acknowledge them rather
			// than leaving the session stopped with a pending notice.
			if err := send.Send(Event{Type: EventDone}); err != nil {
				return err
			}
			return nil
		}

		if attempt == maxTurns-1 {
			if err := send.Send(Event{Type: EventPhase, Text: MaxTurnsExceededWarning(maxTurns)}); err != nil {
				return err
			}
			return &MaxTurnsExceededError{MaxTurns: maxTurns}
		}

		// Build assistant message with text + tool calls + reasoning
		// (built before tool execution so we can save it incrementally)
		assistantMsg := buildAssistantMessageWithReasoningMetadata(
			textBuilder.String(),
			e.withToolPreview(registered),
			reasoningBuilder.String(),
			reasoningSummaryParts,
			reasoningItemID,
			reasoningEncryptedContent,
			reasoningKind,
		)

		maybeCompactAfterLLMCall([]Message{assistantMsg})

		// Call responseCallback BEFORE tool execution to persist assistant message
		// This ensures the message is saved even if tool execution fails/crashes
		responseHandled := callResponseCompletedCallback(ctx, responseCallback, attempt, assistantMsg, turnMetrics)

		// ToolMap: swap client tool names to mapped server names for execution.
		// We save original names keyed by call ID so we can restore them on
		// the registered slice and the tool-result messages afterwards.
		var origNameByID map[string]string
		if req.ToolMap != nil {
			for i := range registered {
				if mapped, ok := req.ToolMap[registered[i].Name]; ok {
					if origNameByID == nil {
						origNameByID = make(map[string]string)
					}
					origNameByID[registered[i].ID] = registered[i].Name
					registered[i].Name = mapped
				}
			}
		}

		// Execute registered tools
		for _, call := range registered {
			DebugToolCall(req.Debug, call)
			info := e.getToolPreview(call)

			if err := send.Send(Event{Type: EventToolExecStart, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: info, ToolArgs: call.Arguments}); err != nil {
				return err
			}
		}

		transcriptForApproval := append(append([]Message(nil), req.ApprovalTranscriptPrefix...), req.Messages...)
		transcriptForApproval = append(transcriptForApproval, assistantMsg)
		toolResults, err := e.executeToolCalls(ctx, registered, req.ParallelToolCalls, send, req.Debug, req.DebugRaw, transcriptForApproval)
		if err != nil {
			return err
		}

		finishingToolExecuted = false
		for _, call := range registered {
			if e.tools.IsFinishingTool(call.Name) {
				finishingToolExecuted = true
				break
			}
		}

		// Restore original (client-facing) names so conversation history
		// references the names the client expects.
		if origNameByID != nil {
			for i := range registered {
				if orig, ok := origNameByID[registered[i].ID]; ok {
					registered[i].Name = orig
				}
			}
			for i := range toolResults {
				for j := range toolResults[i].Parts {
					if toolResults[i].Parts[j].ToolResult != nil {
						if orig, ok := origNameByID[toolResults[i].Parts[j].ToolResult.ID]; ok {
							toolResults[i].Parts[j].ToolResult.Name = orig
						}
					}
				}
			}
		}

		req.Messages = append(req.Messages, assistantMsg)
		req.Messages = append(req.Messages, toolResults...)
		if err := applyPendingRequestModelSwitch(attempt + 1); err != nil {
			return err
		}

		// Call turn completed callback with tool results for incremental persistence
		if turnCallback != nil {
			turnMetrics.ToolCalls = len(registered)
			turnMessages := turnMessagesAfterResponseCallback(responseHandled, assistantMsg, toolResults)
			cbCtx, cancel := callbackContext(ctx)
			_ = turnCallback(cbCtx, attempt, turnMessages, turnMetrics)
			cancel()
		}

		// Exit promptly if caller cancelled while tools were executing.
		// Check after the turn callback so in-progress tool results are persisted
		// before we abandon the loop.
		if err := ctx.Err(); err != nil {
			return err
		}

		if finishingToolExecuted {
			if err := send.Send(Event{Type: EventDone}); err != nil {
				return err
			}
			return nil
		}

		// Check for user interjections queued during this turn.
		// If present, inject them as FIFO user messages so the LLM sees them on the next turn.
		if interjections := e.drainInterjections(); len(interjections) > 0 {
			interjectionMsgs := make([]Message, 0, len(interjections))
			for _, interjection := range interjections {
				interjectionMsg := interjection.Message
				interjectionMsg.Role = RoleUser
				req.Messages = append(req.Messages, interjectionMsg)
				interjectionMsgs = append(interjectionMsgs, interjectionMsg)
			}
			// Fire turn callback so interjections are persisted
			if turnCallback != nil {
				cbCtx, cancel := callbackContext(ctx)
				_ = turnCallback(cbCtx, attempt, interjectionMsgs, TurnMetrics{})
				cancel()
			}
			// Emit events so UIs can display committed interjections inline
			for _, interjection := range interjections {
				text := interjection.DisplayText
				if text == "" {
					text = MessageText(interjection.Message)
				}
				if err := send.Send(Event{Type: EventInterjection, Text: text, InterjectionID: interjection.ID, Message: interjection.Message, InterjectionStatus: InterjectionCommitted}); err != nil {
					return err
				}
			}
			if err := applyPendingRequestModelSwitch(attempt + 1); err != nil {
				return err
			}
		}
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

// executeToolCalls executes multiple tool calls, potentially in parallel.
// Note: When executing in parallel, EventToolExecStart/EventToolExecEnd events
// are emitted from concurrent goroutines. While the channel is thread-safe, events
// may arrive in non-deterministic order. Consumers should use ToolCallID to correlate
// start/end events rather than relying on ordering.
func (e *Engine) executeToolCalls(ctx context.Context, calls []ToolCall, parallel bool, send eventSender, debug bool, debugRaw bool, transcriptOpt ...[]Message) ([]Message, error) {
	var transcript []Message
	if len(transcriptOpt) > 0 {
		transcript = transcriptOpt[0]
	}
	// Cancellation must still yield a result message for every announced call:
	// the caller persists the assistant message with its tool calls, and a turn
	// with dangling tool calls breaks conversation resume on strict providers.
	if err := ctx.Err(); err != nil {
		return cancelledToolCallMessages(calls, err), nil
	}

	// Fast path: single call, no concurrency overhead
	if len(calls) == 1 {
		return e.executeSingleToolCallSafe(ContextWithApprovalTranscript(ctx, transcript), calls[0], send, debug, debugRaw)
	}

	if !parallel {
		results := make([]Message, 0, len(calls))
		toolCtx := ContextWithApprovalTranscript(ctx, transcript)
		for i, call := range calls {
			if err := ctx.Err(); err != nil {
				return append(results, cancelledToolCallMessages(calls[i:], err)...), nil
			}
			msgs, err := e.executeSingleToolCallSafe(toolCtx, call, send, debug, debugRaw)
			if err != nil {
				return nil, err
			}
			msg := ToolErrorMessage(call.ID, call.Name, "tool returned no result", call.ThoughtSig)
			if len(msgs) > 0 {
				msg = msgs[0]
			}
			results = append(results, msg)
		}
		return results, nil
	}

	// Parallel execution for multiple calls (events may arrive out of order), but
	// cap worker count so a single model turn cannot flood the process with tool
	// executions all at once.
	type toolResult struct {
		index   int
		message Message
	}

	resultChan := make(chan toolResult, len(calls))
	workerCount := maxParallelToolWorkers(len(calls))
	var nextCall atomic.Uint32

	workerCtx := ContextWithApprovalTranscript(ctx, transcript)
	for worker := 0; worker < workerCount; worker++ {
		go func() {
			for {
				if err := ctx.Err(); err != nil {
					return
				}

				idx := int(nextCall.Add(1)) - 1
				if idx >= len(calls) {
					return
				}

				if err := ctx.Err(); err != nil {
					return
				}

				call := calls[idx]
				msgs, _ := e.executeSingleToolCallSafe(workerCtx, call, send, debug, debugRaw)
				msg := ToolErrorMessage(call.ID, call.Name, "tool returned no result", call.ThoughtSig)
				if len(msgs) > 0 {
					msg = msgs[0]
				}
				resultChan <- toolResult{index: idx, message: msg}
			}
		}()
	}

	// Collect results and maintain original order. If the caller cancels while
	// non-cooperative tools are still running, return promptly instead of waiting
	// for every goroutine to finish. The buffered channel is sized for one result
	// per tool, so late tool completions cannot block after cancellation.
	results := make([]Message, len(calls))
	for remaining := len(calls); remaining > 0; remaining-- {
		select {
		case r := <-resultChan:
			results[r.index] = r.message
		case <-ctx.Done():
			// Keep any results that finished before cancellation, then
			// synthesize cancelled results for the rest so every announced
			// call stays paired with a result in the persisted turn.
			for drained := false; !drained; {
				select {
				case r := <-resultChan:
					results[r.index] = r.message
				default:
					drained = true
				}
			}
			for i := range results {
				if results[i].Role == "" {
					results[i] = cancelledToolCallMessage(calls[i], ctx.Err())
				}
			}
			return results, nil
		}
	}

	return results, nil
}

// cancelledToolCallMessage synthesizes the error result for a tool call that
// was skipped or abandoned because the context was cancelled.
func cancelledToolCallMessage(call ToolCall, err error) Message {
	return ToolErrorMessage(call.ID, call.Name, fmt.Sprintf("Error: %v", err), call.ThoughtSig)
}

func cancelledToolCallMessages(calls []ToolCall, err error) []Message {
	msgs := make([]Message, 0, len(calls))
	for _, call := range calls {
		msgs = append(msgs, cancelledToolCallMessage(call, err))
	}
	return msgs
}

// executeSingleToolCallSafe wraps executeSingleToolCall with panic recovery.
func (e *Engine) executeSingleToolCallSafe(ctx context.Context, call ToolCall, send eventSender, debug bool, debugRaw bool) (msgs []Message, err error) {
	defer func() {
		if r := recover(); r != nil {
			errMsg := fmt.Sprintf("Error: tool panicked: %v", r)
			send.TrySend(Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolSuccess: false})
			msgs = []Message{ToolErrorMessage(call.ID, call.Name, errMsg, call.ThoughtSig)}
			err = nil
		}
	}()
	return e.executeSingleToolCall(ctx, call, send, debug, debugRaw)
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

// executeSingleToolCall executes a single tool call and returns the result message.
func (e *Engine) executeSingleToolCall(ctx context.Context, call ToolCall, send eventSender, debug bool, debugRaw bool) ([]Message, error) {
	tool, ok := e.tools.Get(call.Name)
	if !ok {
		errMsg := fmt.Sprintf("Error: tool not registered: %s", call.Name)
		DebugToolResult(debug, call.ID, call.Name, errMsg)
		send.TrySend(Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolSuccess: false})
		return []Message{ToolErrorMessage(call.ID, call.Name, errMsg, call.ThoughtSig)}, nil
	}

	// Check if tool is allowed under current skill restrictions
	if !e.IsToolAllowed(call.Name) {
		errMsg := fmt.Sprintf("Error: tool '%s' is not in the active skill's allowed-tools list", call.Name)
		DebugToolResult(debug, call.ID, call.Name, errMsg)
		send.TrySend(Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolSuccess: false})
		return []Message{ToolErrorMessage(call.ID, call.Name, errMsg, call.ThoughtSig)}, nil
	}

	// Add call ID to context for spawn_agent event bubbling
	toolCtx := ContextWithCallID(ctx, call.ID)

	stopHeartbeat := startToolHeartbeat(ctx, call.ID, call.Name, send)
	defer stopHeartbeat()

	output, err := tool.Execute(toolCtx, call.Arguments)
	info := e.getToolPreview(call)

	// Truncate large tool outputs (global limit, then compaction limit).
	if err == nil {
		output.Content = e.applyToolOutputTruncation(output.Content)
	}

	if err != nil {
		errMsg := fmt.Sprintf("Error: %v", err)
		DebugToolResult(debug, call.ID, call.Name, errMsg)
		send.TrySend(Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: info, ToolSuccess: false})
		return []Message{ToolErrorMessage(call.ID, call.Name, errMsg, call.ThoughtSig)}, nil
	}

	DebugToolResult(debug, call.ID, call.Name, output.Content)
	DebugRawToolResult(debugRaw, call.ID, call.Name, output.Content)
	// Best-effort: don't let a slow event consumer stall completed tool workers.
	send.TrySend(Event{
		Type:            EventToolExecEnd,
		ToolCallID:      call.ID,
		ToolName:        call.Name,
		ToolInfo:        info,
		ToolSuccess:     !output.TimedOut && !output.IsError,
		ToolOutput:      output.Content,
		ToolDiffs:       output.Diffs,
		ToolFileChanges: output.FileChanges,
		ToolImages:      output.Images,
	})
	return []Message{ToolResultMessageFromOutput(call.ID, call.Name, output, call.ThoughtSig)}, nil
}

// handleSyncToolExecution handles synchronous tool execution for bridged providers.
// It emits EventToolExecStart/End to the outer channel (for TUI) and sends the result
// back to the provider via the response channel.
// Returns the tool call, result content string, and any error that occurred during execution.
func (e *Engine) handleSyncToolExecution(ctx context.Context, event Event, send eventSender, debug bool, debugRaw bool) (ToolCall, ToolOutput, error) {
	call := event.Tool
	callID := event.ToolCallID
	if callID == "" {
		callID = call.ID
	}

	// Get tool preview info
	info := e.getToolPreview(*call)
	if event.ToolInfo != "" {
		info = event.ToolInfo
	}

	// Emit start event to TUI (non-blocking to avoid deadlock if consumer is slow)
	send.TrySend(Event{
		Type:       EventToolExecStart,
		ToolCallID: callID,
		ToolName:   call.Name,
		ToolInfo:   info,
		ToolArgs:   call.Arguments,
	})

	// Look up and execute the tool
	tool, ok := e.tools.Get(call.Name)
	var result ToolOutput
	var err error

	if !ok {
		// suggest_commands is a passthrough tool - it captures structured output
		// and doesn't need actual execution. Just return success.
		if call.Name == SuggestCommandsToolName {
			result = TextOutput("OK")
		} else {
			err = fmt.Errorf("tool not found: %s", call.Name)
		}
	} else if !e.IsToolAllowed(call.Name) {
		err = fmt.Errorf("tool '%s' is not in the active skill's allowed-tools list", call.Name)
	} else {
		toolCtx := ContextWithCallID(ctx, callID)
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("Error: tool panicked: %v", r)
				}
			}()
			result, err = tool.Execute(toolCtx, call.Arguments)
		}()
	}

	// Truncate large tool outputs (global limit, then compaction limit).
	if err == nil {
		result.Content = e.applyToolOutputTruncation(result.Content)
	}

	// Debug logging
	if err != nil {
		DebugToolResult(debug, callID, call.Name, fmt.Sprintf("Error: %v", err))
	} else {
		DebugToolResult(debug, callID, call.Name, result.Content)
		DebugRawToolResult(debugRaw, callID, call.Name, result.Content)
	}
	// Emit end event to TUI (non-blocking to avoid deadlock if consumer is slow)
	send.TrySend(Event{
		Type:            EventToolExecEnd,
		ToolCallID:      callID,
		ToolName:        call.Name,
		ToolInfo:        info,
		ToolSuccess:     err == nil && !result.TimedOut && !result.IsError,
		ToolOutput:      result.Content,
		ToolDiffs:       result.Diffs,
		ToolFileChanges: result.FileChanges,
		ToolImages:      result.Images,
	})

	// Send the result back to the provider bridge.
	// Use select to avoid blocking if context is canceled and receiver has exited
	select {
	case event.ToolResponse <- ToolExecutionResponse{Result: result, Err: err}:
	case <-ctx.Done():
		// Best-effort: abandon send if context canceled
	}

	// Ensure call has the proper ID (may have been generated)
	returnCall := *call
	returnCall.ID = callID
	returnCall.ToolInfo = info
	return returnCall, result, err
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
			calls[i].ID = fmt.Sprintf("toolcall-%d", i+1)
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
	trackedExternal string // "claude-code", "codex", "gemini-cli", or "" for direct API

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
