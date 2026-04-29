package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/usage"
)

const (
	defaultMaxTurns       = 20
	stopSearchToolHint    = "IMPORTANT: Do not call any tools. Use the information already retrieved and answer directly."
	callbackTimeout       = 5 * time.Second
	toolHeartbeatInterval = 10 * time.Second
)

// getMaxTurns returns the max turns from request, with fallback to default
func getMaxTurns(req Request) int {
	if req.MaxTurns > 0 {
		return req.MaxTurns
	}
	return defaultMaxTurns
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
// generated during that turn and metrics about the turn.
// turnIndex is 0-based, messages contains assistant message(s) and tool result(s).
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
// update their state (e.g., replace in-memory messages, persist changes).
type CompactionCallback func(ctx context.Context, result *CompactionResult) error

// Engine orchestrates provider calls and external tool execution.
type Engine struct {
	provider    Provider
	tools       *ToolRegistry
	debugLogger *DebugLogger

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
	compactionConfig         *CompactionConfig // nil = compaction disabled
	inputLimit               int               // 0 = unknown/disabled
	lastTotalTokens          int               // cached+input+output from most recent API response
	lastMessageCount         int               // len(req.Messages) at time of last API call
	lastMessageTokenEstimate int               // heuristic token estimate of messages included in lastTotalTokens
	systemPrompt             string            // Captured for re-injection after compaction
	contextNoticeEmitted     atomic.Bool       // one-shot flag: WARNING emitted once per session

	// Interjection support: user can send a message while the agent is streaming.
	// The message is injected after the current turn's tool results, before the next LLM turn.
	interjection chan queuedInterjection // Buffered channel (size 1) for mid-stream user interjections

	// pendingToolSpecs holds tool specs registered mid-loop (e.g. via skill activation)
	// that should be injected into req.Tools at the start of the next loop iteration.
	pendingToolSpecs []ToolSpec
	pendingToolsMu   sync.Mutex
}

// ToolExecutorSetter is an optional interface for providers that need
// tool execution wired up externally (e.g., claude-bin with HTTP MCP).
type ToolExecutorSetter interface {
	SetToolExecutor(func(ctx context.Context, name string, args json.RawMessage) (ToolOutput, error))
}

// ProviderCleaner is an optional interface for providers that need cleanup
// after a conversation ends (e.g., claude-bin's persistent MCP server).
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

type queuedInterjection struct {
	ID   string
	Text string
}

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

	// Wire up tool executor for providers that need it (e.g., claude-bin HTTP MCP)
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

// ResetConversation clears all conversation-specific state from the engine.
// Called on /clear or /new to start a fresh conversation. This resets
// compaction tracking, context notices, and provider-side conversation state
// (e.g., OpenAI Responses API previous_response_id).
func (e *Engine) ResetConversation() {
	e.callbackMu.Lock()
	e.lastTotalTokens = 0
	e.lastMessageCount = 0
	e.lastMessageTokenEstimate = 0
	e.systemPrompt = ""
	e.contextNoticeEmitted.Store(false)
	e.callbackMu.Unlock()

	// Reset provider-side conversation state if supported
	type conversationResetter interface {
		ResetConversation()
	}
	if r, ok := e.provider.(conversationResetter); ok {
		r.ResetConversation()
	}
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

	if !provider.Capabilities().ManagesOwnContext {
		limit = InputLimitForProviderModel(providerName, modelName)
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

// ContextEstimateBaseline returns the persisted context-estimate baseline:
// the last observed total tokens and the message count at which it was observed.
func (e *Engine) ContextEstimateBaseline() (int, int) {
	e.callbackMu.RLock()
	total := e.lastTotalTokens
	count := e.lastMessageCount
	e.callbackMu.RUnlock()
	return total, count
}

// SetContextEstimateBaseline seeds the context-estimate baseline, typically
// from persisted session state on resume.
func (e *Engine) SetContextEstimateBaseline(lastTotalTokens, lastMessageCount int) {
	if lastTotalTokens < 0 {
		lastTotalTokens = 0
	}
	if lastMessageCount < 0 {
		lastMessageCount = 0
	}
	e.callbackMu.Lock()
	e.lastTotalTokens = lastTotalTokens
	e.lastMessageCount = lastMessageCount
	e.lastMessageTokenEstimate = 0
	e.callbackMu.Unlock()
}

// EstimateTokens returns the estimated input token count for the next API call
// based on the current message list. It uses the most recent API usage as a
// baseline when possible, then adds heuristic estimates for newly appended
// messages.
func (e *Engine) EstimateTokens(messages []Message) int {
	e.callbackMu.RLock()
	lastTotalTokens := e.lastTotalTokens
	lastMessageCount := e.lastMessageCount
	lastMessageTokenEstimate := e.lastMessageTokenEstimate
	e.callbackMu.RUnlock()

	if lastTotalTokens > 0 && lastMessageTokenEstimate > 0 {
		// Prefer token-estimate checkpoints over message-count checkpoints. The
		// provider baseline already includes the prompt plus assistant output from
		// the checkpointed turn; only add heuristic tokens for content that appears
		// after that checkpoint. This is robust to message-count drift from
		// persistence details (e.g. assistant rows being upserted/reloaded).
		if delta := EstimateMessageTokens(messages) - lastMessageTokenEstimate; delta > 0 {
			return lastTotalTokens + delta
		}
		return lastTotalTokens
	}

	if lastTotalTokens > 0 && lastMessageCount > 0 {
		switch {
		case lastMessageCount == len(messages):
			return lastTotalTokens
		case lastMessageCount < len(messages):
			return lastTotalTokens + EstimateMessageTokens(messages[lastMessageCount:])
		case lastMessageCount > len(messages):
			// The latest provider usage can arrive before the UI/session has appended
			// the assistant output that the usage already includes. In that transient
			// state, the provider baseline is more accurate than falling back to a full
			// heuristic estimate, which can badly inflate the status-line meter.
			return lastTotalTokens
		}
	}
	return EstimateMessageTokens(messages)
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

// Interject queues a user message to be inserted after the current turn's tool results,
// right before the next LLM turn begins. Non-blocking: if an interjection is already
// pending, the new one replaces it (only the latest interjection is kept).
// Safe to call from any goroutine (e.g., the TUI thread).
func (e *Engine) Interject(text string) {
	e.InterjectWithID("", text)
}

// InterjectWithID behaves like Interject but preserves a caller-supplied stable
// identifier. This lets higher layers match the eventual EventInterjection back
// to the pending UI row they rendered while classification was in-flight.
func (e *Engine) InterjectWithID(id, text string) {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()

	if e.interjection == nil {
		e.interjection = make(chan queuedInterjection, 1)
	}
	ch := e.interjection
	if strings.TrimSpace(id) == "" {
		id = nextEngineInterjectionID()
	}
	entry := queuedInterjection{ID: id, Text: text}

	// Drain-then-send: replace any pending interjection with the new one.
	select {
	case <-ch:
	default:
	}
	ch <- entry
}

// DrainInterjection returns the pending interjection text, or "" if none.
// Non-blocking. Public so the TUI layer can recover a pending interjection
// when the stream completes without tool calls (the "between turns" injection
// point was never reached). The recovered text can be placed back in the textarea.
func (e *Engine) DrainInterjection() string {
	return e.drainInterjection().Text
}

// PeekInterjection returns the currently pending interjection text without
// consuming it. Returns "" if none. Safe to call from any goroutine.
func (e *Engine) PeekInterjection() string {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()

	if e.interjection == nil {
		return ""
	}
	ch := e.interjection
	select {
	case entry := <-ch:
		// Put it back. We hold the write lock, so no concurrent Interject
		// can fill the (size-1) buffer between the receive and re-send.
		ch <- entry
		return entry.Text
	default:
		return ""
	}
}

// drainInterjection returns the pending interjection entry, or a zero value if none.
// Non-blocking. Called within runLoop between turns.
//
// Takes the exclusive lock (matching Interject and PeekInterjection) so a
// concurrent PeekInterjection cannot temporarily empty the channel between
// our channel-read and the peek's put-back, which would cause us to return an
// empty entry when an interjection was actually pending.
func (e *Engine) drainInterjection() queuedInterjection {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()

	if e.interjection == nil {
		return queuedInterjection{}
	}
	select {
	case entry := <-e.interjection:
		return entry
	default:
		return queuedInterjection{}
	}
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
// call. Uses total_tokens (input+output) from the last API response as a
// baseline — because the model's output gets echoed back as input on the
// next turn — then adds heuristic estimates for messages appended since.
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

// Stream returns a stream, applying external tools when needed.
func (e *Engine) Stream(ctx context.Context, req Request) (Stream, error) {
	if req.DebugRaw {
		DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), req, "Request")
	}

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

	// 2. Decide if we use the agentic loop
	// We use it if request has tools AND provider supports tool calls
	useLoop := len(req.Tools) > 0 && caps.ToolCalls

	if useLoop {
		stream := newEventStream(ctx, func(ctx context.Context, send eventSender) error {
			return e.runLoop(ctx, req, send)
		})
		stream = wrapLoggingStream(stream, e.provider.Name(), req.Model)
		stream = e.wrapDebugLoggingStream(stream)

		// Wrap with per-turn cleanup for providers that need it (e.g. claude-bin
		// temp image files). Conversation-scoped cleanup (CleanupMCP) is NOT
		// invoked here — it runs on runtime eviction / server shutdown.
		if cleaner, ok := e.provider.(ProviderTurnCleaner); ok {
			stream = &cleanupStream{inner: stream, cleanup: cleaner.CleanupTurn}
		}

		return stream, nil
	}

	// 3. Simple stream (no tools or no provider support for tools)
	// Log request for non-agentic requests too
	if e.debugLogger != nil {
		e.debugLogger.LogRequest(e.provider.Name(), req.Model, req)
	}

	stream, err := e.provider.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	stream = WrapDebugStream(req.DebugRaw, stream)
	stream = wrapLoggingStream(stream, e.provider.Name(), req.Model)

	// Wrap to call turn callback even for simple streams
	// Copy callback under lock to avoid race with SetTurnCompletedCallback
	if cb := e.getTurnCallback(); cb != nil {
		stream = wrapCallbackStream(ctx, stream, cb)
	}

	return e.wrapDebugLoggingStream(stream), nil
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
	}
}

// callbackStream wraps a stream to accumulate text/usage and call callback on EOF.
type callbackStream struct {
	inner              Stream
	ctx                context.Context
	mu                 sync.Mutex
	text               *strings.Builder
	reasoning          *strings.Builder
	reasoningItemID    string
	reasoningEncrypted string
	metrics            TurnMetrics
	callback           TurnCompletedCallback
	done               bool
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
		}
		if event.ReasoningItemID != "" {
			s.reasoningItemID = event.ReasoningItemID
		}
		if event.ReasoningEncryptedContent != "" {
			s.reasoningEncrypted = event.ReasoningEncryptedContent
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
	if s.callback != nil && !s.done && (s.text.Len() > 0 || s.reasoning.Len() > 0 || s.reasoningItemID != "" || s.reasoningEncrypted != "") {
		s.done = true
		cb = s.callback
		msg = Message{
			Role: RoleAssistant,
			Parts: []Part{{
				Type:                      PartText,
				Text:                      s.text.String(),
				ReasoningContent:          s.reasoning.String(),
				ReasoningItemID:           s.reasoningItemID,
				ReasoningEncryptedContent: s.reasoningEncrypted,
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

func (e *Engine) runLoop(ctx context.Context, req Request, send eventSender) error {
	maxTurns := getMaxTurns(req)
	originalToolChoice := req.ToolChoice
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
	applyCompaction := func(result *CompactionResult) bool {
		if cb := e.getCompactionCallback(); cb != nil {
			if cbErr := cb(ctx, result); cbErr != nil {
				slog.Warn("compaction callback failed", "error", cbErr)
				return false
			}
		}
		req.Messages = result.NewMessages
		e.callbackMu.Lock()
		e.lastTotalTokens = 0
		e.lastMessageCount = 0
		e.lastMessageTokenEstimate = 0
		e.callbackMu.Unlock()
		return true
	}
	for attempt := 0; attempt < maxTurns; attempt++ {
		// Inject any tool specs registered mid-loop (e.g. via skill activation)
		if pending := e.drainPendingToolSpecs(); len(pending) > 0 {
			for _, spec := range pending {
				if !hasToolNamed(req.Tools, spec.Name) {
					req.Tools = append(req.Tools, spec)
				}
			}
		}

		// Pre-turn compaction check (skip first turn — no history to compact yet)
		if compactionConfig != nil && attempt > 0 {
			threshold := int(float64(inputLimit) * compactionConfig.ThresholdRatio)
			if e.estimatedTokens(req.Messages) >= threshold {
				if err := send.Send(Event{Type: EventPhase, Text: "Compacting context..."}); err != nil {
					return err
				}
				result, err := Compact(ctx, e.provider, req.Model, systemPrompt, nonSystemMessages(req.Messages), *compactionConfig)
				if err == nil {
					applyCompaction(result)
				}
				// On error: continue with full context (best effort)
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
		} else if attempt > 0 {
			// Ensure we are in Auto mode for follow-up turns in the loop
			req.ToolChoice = ToolChoice{Mode: ToolChoiceAuto}
		}

		// Log per-turn request state
		// For attempt 0: captures state after applyExternalSearch modifications
		// For attempt > 0: captures tool results appended in previous turn
		if e.debugLogger != nil {
			e.debugLogger.LogTurnRequest(attempt, e.provider.Name(), req.Model, req)
		}

		if req.DebugRaw {
			DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), req, fmt.Sprintf("Request (turn %d)", attempt))
		}

		stream, err := e.provider.Stream(ctx, req)
		if err != nil {
			// Reactive compaction: if this is a context overflow error, try compacting and retrying (once)
			if compactionConfig != nil && isContextOverflowError(err) && !reactiveCompactionDone {
				reactiveCompactionDone = true
				if err := send.Send(Event{Type: EventPhase, Text: "Compacting context..."}); err != nil {
					return err
				}
				result, compactErr := Compact(ctx, e.provider, req.Model, systemPrompt, nonSystemMessages(req.Messages), *compactionConfig)
				if compactErr == nil && applyCompaction(result) {
					attempt-- // Retry this turn
					continue
				}
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
		var turnMetrics TurnMetrics
		var syncToolsExecuted bool     // Track if tools were executed via sync path (MCP)
		var finishingToolExecuted bool // Track if a finishing tool was executed (agent done)
		var syncToolCalls []ToolCall   // Track sync tool calls for message building
		var syncToolResults []Message  // Track sync tool results for message building
		persistPartialAssistant := func() {
			hasTextOrReasoning := textBuilder.Len() > 0 || reasoningBuilder.Len() > 0 || reasoningItemID != "" || reasoningEncryptedContent != ""
			if !hasTextOrReasoning && len(toolCalls) == 0 && len(syncToolCalls) == 0 {
				return
			}

			if len(toolCalls) == 0 {
				if syncToolsExecuted {
					assistantMsg := buildAssistantMessageWithReasoningMetadata(
						textBuilder.String(),
						e.withToolPreview(syncToolCalls),
						reasoningBuilder.String(),
						reasoningItemID,
						reasoningEncryptedContent,
					)
					if turnCallback != nil {
						turnMetrics.ToolCalls = len(syncToolCalls)
						turnMessages := []Message{assistantMsg}
						turnMessages = append(turnMessages, syncToolResults...)
						cbCtx, cancel := callbackContext(ctx)
						_ = turnCallback(cbCtx, attempt, turnMessages, turnMetrics)
						cancel()
					}
					return
				}
				if turnCallback != nil && hasTextOrReasoning {
					finalMsg := Message{
						Role: RoleAssistant,
						Parts: []Part{{
							Type:                      PartText,
							Text:                      textBuilder.String(),
							ReasoningContent:          reasoningBuilder.String(),
							ReasoningItemID:           reasoningItemID,
							ReasoningEncryptedContent: reasoningEncryptedContent,
						}},
					}
					cbCtx, cancel := callbackContext(ctx)
					_ = turnCallback(cbCtx, attempt, []Message{finalMsg}, turnMetrics)
					cancel()
				}
				return
			}

			partialToolCalls := ensureToolCallIDs(toolCalls)
			partialToolCalls = dedupeToolCalls(partialToolCalls)
			assistantMsg := buildAssistantMessageWithReasoningMetadata(
				textBuilder.String(),
				e.withToolPreview(partialToolCalls),
				reasoningBuilder.String(),
				reasoningItemID,
				reasoningEncryptedContent,
			)
			if len(assistantMsg.Parts) == 0 {
				return
			}
			if responseCallback != nil {
				cbCtx, cancel := callbackContext(ctx)
				_ = responseCallback(cbCtx, attempt, assistantMsg, turnMetrics)
				cancel()
				return
			}
			if turnCallback != nil {
				cbCtx, cancel := callbackContext(ctx)
				_ = turnCallback(cbCtx, attempt, []Message{assistantMsg}, turnMetrics)
				cancel()
			}
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
				reasoningItemID,
				reasoningEncryptedContent,
			)
			if len(msg.Parts) == 0 {
				return
			}
			cbCtx, cancel := callbackContext(ctx)
			_ = snapshotCallback(cbCtx, attempt, msg)
			cancel()
		}
		for {
			event, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				stream.Close()
				persistPartialAssistant()
				return err
			}
			if event.Type == EventError && event.Err != nil {
				stream.Close()
				persistPartialAssistant()
				return event.Err
			}
			if req.DebugRaw {
				DebugRawEvent(true, event)
			}
			// Track usage metrics
			if event.Type == EventUsage && event.Use != nil {
				turnMetrics.InputTokens += event.Use.InputTokens
				turnMetrics.OutputTokens += event.Use.OutputTokens
				turnMetrics.CachedInputTokens += event.Use.CachedInputTokens
				turnMetrics.CacheWriteTokens += event.Use.CacheWriteTokens
				// Update token tracking for compaction threshold and status line display.
				// InputTokens is the non-cached portion; CachedInputTokens is the cached
				// portion. Together they equal the total context size this turn. Adding
				// OutputTokens gives the baseline for the next turn's input estimate
				// (the model's output becomes assistant-message input on the next turn).
				// All providers normalise to this convention — see Usage type docs.
				if inputLimit > 0 {
					checkpointMessages := append([]Message(nil), req.Messages...)
					messageCount := len(checkpointMessages)
					// Usage total includes the assistant output from this provider turn.
					// That output becomes assistant-message input on the next request, so
					// include the accumulated assistant state in the checkpoint. Later
					// estimates can then add only heuristic deltas that appear after this
					// checkpoint (for example tool results or a new user message).
					if event.Use.OutputTokens > 0 || textBuilder.Len() > 0 || reasoningBuilder.Len() > 0 || len(toolCalls) > 0 || len(syncToolCalls) > 0 || reasoningItemID != "" || reasoningEncryptedContent != "" {
						assistantCalls := toolCalls
						if len(assistantCalls) == 0 && len(syncToolCalls) > 0 {
							assistantCalls = syncToolCalls
						}
						assistantMsg := buildAssistantMessageWithReasoningMetadata(
							textBuilder.String(),
							e.withToolPreview(ensureToolCallIDs(dedupeToolCalls(assistantCalls))),
							reasoningBuilder.String(),
							reasoningItemID,
							reasoningEncryptedContent,
						)
						if len(assistantMsg.Parts) > 0 {
							checkpointMessages = append(checkpointMessages, assistantMsg)
							messageCount = len(checkpointMessages)
						} else if event.Use.OutputTokens > 0 {
							messageCount++
						}
					}
					e.callbackMu.Lock()
					e.lastTotalTokens = event.Use.InputTokens + event.Use.CachedInputTokens + event.Use.OutputTokens
					e.lastMessageCount = messageCount
					e.lastMessageTokenEstimate = EstimateMessageTokens(checkpointMessages)
					e.callbackMu.Unlock()
				}
			}
			// Accumulate text for callback
			if event.Type == EventTextDelta && event.Text != "" {
				textBuilder.WriteString(event.Text)
			}
			// Accumulate reasoning for thinking models (OpenRouter)
			if event.Type == EventReasoningDelta && event.Text != "" {
				reasoningBuilder.WriteString(event.Text)
			}
			if event.Type == EventReasoningDelta {
				if event.ReasoningItemID != "" {
					reasoningItemID = event.ReasoningItemID
				}
				if event.ReasoningEncryptedContent != "" {
					reasoningEncryptedContent = event.ReasoningEncryptedContent
				}
			}
			if event.Type == EventToolCall && event.Tool != nil {
				// Check if this is a synchronous tool execution request (from claude_bin MCP)
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

		// Search is only performed once (either pre-emptively or in first turn)
		req.Search = false

		if len(toolCalls) == 0 && !syncToolsExecuted {
			// No tools called - check if we should restore original tool choice and retry once
			if originalToolChoice.Mode == ToolChoiceName && !restoredToolChoice {
				req.ToolChoice = originalToolChoice
				restoredToolChoice = true
				continue
			}
			// Call turnCallback with final text-only response (no tools)
			// Note: responseCallback is NOT called here because no tool execution follows.
			// responseCallback is only for persisting assistant messages before tool execution.
			if turnCallback != nil && (textBuilder.Len() > 0 || reasoningBuilder.Len() > 0 || reasoningItemID != "" || reasoningEncryptedContent != "") {
				finalMsg := Message{
					Role: RoleAssistant,
					Parts: []Part{{
						Type:                      PartText,
						Text:                      textBuilder.String(),
						ReasoningContent:          reasoningBuilder.String(),
						ReasoningItemID:           reasoningItemID,
						ReasoningEncryptedContent: reasoningEncryptedContent,
					}},
				}
				cbCtx, cancel := callbackContext(ctx)
				_ = turnCallback(cbCtx, attempt, []Message{finalMsg}, turnMetrics)
				cancel()
			}
			if err := send.Send(Event{Type: EventDone}); err != nil {
				return err
			}
			return nil
		}

		// If only sync tools were executed (MCP path), decide whether to continue
		if len(toolCalls) == 0 && syncToolsExecuted {
			// Build assistant message with text and sync tool calls
			// This is needed so claude-bin gets proper context when resuming
			assistantMsg := buildAssistantMessageWithReasoningMetadata(
				textBuilder.String(),
				e.withToolPreview(syncToolCalls),
				reasoningBuilder.String(),
				reasoningItemID,
				reasoningEncryptedContent,
			)
			req.Messages = append(req.Messages, assistantMsg)
			req.Messages = append(req.Messages, syncToolResults...)

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

			// Check for user interjection (MCP sync path)
			if interjection := e.drainInterjection(); interjection.Text != "" {
				interjectionMsg := UserText(interjection.Text)
				req.Messages = append(req.Messages, interjectionMsg)
				if turnCallback != nil {
					cbCtx, cancel := callbackContext(ctx)
					_ = turnCallback(cbCtx, attempt, []Message{interjectionMsg}, TurnMetrics{})
					cancel()
				}
				if err := send.Send(Event{Type: EventInterjection, Text: interjection.Text, InterjectionID: interjection.ID}); err != nil {
					return err
				}
			}

			// If a finishing tool was executed, we're done (agent completed its task)
			if finishingToolExecuted {
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
			if turnCallback != nil {
				unregisteredWithInfo := e.withToolPreview(unregistered)
				var parts []Part
				if textBuilder.Len() > 0 || reasoningBuilder.Len() > 0 || reasoningItemID != "" || reasoningEncryptedContent != "" {
					parts = append(parts, Part{
						Type:                      PartText,
						Text:                      textBuilder.String(),
						ReasoningContent:          reasoningBuilder.String(),
						ReasoningItemID:           reasoningItemID,
						ReasoningEncryptedContent: reasoningEncryptedContent,
					})
				}
				for i := range unregisteredWithInfo {
					call := unregisteredWithInfo[i]
					parts = append(parts, Part{Type: PartToolCall, ToolCall: &call})
				}
				if len(parts) > 0 {
					finalMsg := Message{Role: RoleAssistant, Parts: parts}
					cbCtx, cancel := callbackContext(ctx)
					_ = turnCallback(cbCtx, attempt, []Message{finalMsg}, turnMetrics)
					cancel()
				}
			}
			if err := send.Send(Event{Type: EventDone}); err != nil {
				return err
			}
			return nil
		}

		if attempt == maxTurns-1 {
			return fmt.Errorf("agentic loop exceeded max turns (%d)", maxTurns)
		}

		// Build assistant message with text + tool calls + reasoning
		// (built before tool execution so we can save it incrementally)
		assistantMsg := buildAssistantMessageWithReasoningMetadata(
			textBuilder.String(),
			e.withToolPreview(registered),
			reasoningBuilder.String(),
			reasoningItemID,
			reasoningEncryptedContent,
		)

		// Call responseCallback BEFORE tool execution to persist assistant message
		// This ensures the message is saved even if tool execution fails/crashes
		if responseCallback != nil {
			cbCtx, cancel := callbackContext(ctx)
			_ = responseCallback(cbCtx, attempt, assistantMsg, turnMetrics)
			cancel()
		}

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

		toolResults, err := e.executeToolCalls(ctx, registered, req.ParallelToolCalls, send, req.Debug, req.DebugRaw)
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

		// Call turn completed callback with tool results for incremental persistence
		if turnCallback != nil {
			turnMetrics.ToolCalls = len(registered)
			cbCtx, cancel := callbackContext(ctx)
			_ = turnCallback(cbCtx, attempt, toolResults, turnMetrics)
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

		// Check for user interjection queued during this turn.
		// If present, inject it as a user message so the LLM sees it on the next turn.
		if interjection := e.drainInterjection(); interjection.Text != "" {
			interjectionMsg := UserText(interjection.Text)
			req.Messages = append(req.Messages, interjectionMsg)
			// Fire turn callback so the interjection is persisted
			if turnCallback != nil {
				cbCtx, cancel := callbackContext(ctx)
				_ = turnCallback(cbCtx, attempt, []Message{interjectionMsg}, TurnMetrics{})
				cancel()
			}
			// Emit event so TUI can display the interjection inline
			if err := send.Send(Event{Type: EventInterjection, Text: interjection.Text, InterjectionID: interjection.ID}); err != nil {
				return err
			}
		}
	}

	return fmt.Errorf("agentic loop ended unexpectedly")
}

// buildAssistantMessage creates an assistant message with text, tool calls, and optional reasoning.
// The reasoning parameter is for thinking models (OpenRouter reasoning_content).
func buildAssistantMessage(text string, toolCalls []ToolCall, reasoning string) Message {
	return buildAssistantMessageWithReasoningMetadata(text, toolCalls, reasoning, "", "")
}

func buildAssistantMessageWithReasoningMetadata(text string, toolCalls []ToolCall, reasoning, reasoningItemID, reasoningEncryptedContent string) Message {
	var parts []Part
	if text != "" || reasoning != "" || reasoningItemID != "" || reasoningEncryptedContent != "" {
		parts = append(parts, Part{
			Type:                      PartText,
			Text:                      text,
			ReasoningContent:          reasoning,
			ReasoningItemID:           reasoningItemID,
			ReasoningEncryptedContent: reasoningEncryptedContent,
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
func (e *Engine) executeToolCalls(ctx context.Context, calls []ToolCall, parallel bool, send eventSender, debug bool, debugRaw bool) ([]Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Fast path: single call, no concurrency overhead
	if len(calls) == 1 {
		return e.executeSingleToolCallSafe(ctx, calls[0], send, debug, debugRaw)
	}

	if !parallel {
		results := make([]Message, 0, len(calls))
		for _, call := range calls {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			msgs, err := e.executeSingleToolCallSafe(ctx, call, send, debug, debugRaw)
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

	// Parallel execution for multiple calls (events may arrive out of order)
	type toolResult struct {
		index   int
		message Message
	}

	resultChan := make(chan toolResult, len(calls))

	for i, call := range calls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		go func(idx int, c ToolCall) {
			defer func() {
				if r := recover(); r != nil {
					errMsg := fmt.Sprintf("Error: tool panicked: %v", r)
					_ = send.Send(Event{Type: EventToolExecEnd, ToolCallID: c.ID, ToolName: c.Name, ToolSuccess: false})
					resultChan <- toolResult{index: idx, message: ToolErrorMessage(c.ID, c.Name, errMsg, c.ThoughtSig)}
				}
			}()
			msgs, _ := e.executeSingleToolCall(ctx, c, send, debug, debugRaw)
			msg := ToolErrorMessage(c.ID, c.Name, "tool returned no result", c.ThoughtSig)
			if len(msgs) > 0 {
				msg = msgs[0]
			}
			resultChan <- toolResult{index: idx, message: msg}
		}(i, call)
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
			return nil, ctx.Err()
		}
	}

	return results, nil
}

// executeSingleToolCallSafe wraps executeSingleToolCall with panic recovery.
func (e *Engine) executeSingleToolCallSafe(ctx context.Context, call ToolCall, send eventSender, debug bool, debugRaw bool) (msgs []Message, err error) {
	defer func() {
		if r := recover(); r != nil {
			errMsg := fmt.Sprintf("Error: tool panicked: %v", r)
			_ = send.Send(Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolSuccess: false})
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
		_ = send.Send(Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolSuccess: false})
		return []Message{ToolErrorMessage(call.ID, call.Name, errMsg, call.ThoughtSig)}, nil
	}

	// Check if tool is allowed under current skill restrictions
	if !e.IsToolAllowed(call.Name) {
		errMsg := fmt.Sprintf("Error: tool '%s' is not in the active skill's allowed-tools list", call.Name)
		DebugToolResult(debug, call.ID, call.Name, errMsg)
		_ = send.Send(Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolSuccess: false})
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
		_ = send.Send(Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: info, ToolSuccess: false})
		return []Message{ToolErrorMessage(call.ID, call.Name, errMsg, call.ThoughtSig)}, nil
	}

	DebugToolResult(debug, call.ID, call.Name, output.Content)
	DebugRawToolResult(debugRaw, call.ID, call.Name, output.Content)
	_ = send.Send(Event{
		Type:        EventToolExecEnd,
		ToolCallID:  call.ID,
		ToolName:    call.Name,
		ToolInfo:    info,
		ToolSuccess: !output.TimedOut,
		ToolOutput:  output.Content,
		ToolDiffs:   output.Diffs,
		ToolImages:  output.Images,
	})
	return []Message{ToolResultMessageFromOutput(call.ID, call.Name, output, call.ThoughtSig)}, nil
}

// handleSyncToolExecution handles synchronous tool execution for providers like claude_bin.
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
		Type:        EventToolExecEnd,
		ToolCallID:  callID,
		ToolName:    call.Name,
		ToolInfo:    info,
		ToolSuccess: err == nil && !result.TimedOut,
		ToolOutput:  result.Content,
		ToolDiffs:   result.Diffs,
		ToolImages:  result.Images,
	})

	// Send result back to provider (claude_bin MCP handler)
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

	s.mu.Lock()
	if err == nil && event.Type == EventUsage && event.Use != nil {
		s.totalInput += event.Use.InputTokens
		s.totalOutput += event.Use.OutputTokens
		s.totalCacheRead += event.Use.CachedInputTokens
		s.totalCacheWrite += event.Use.CacheWriteTokens
	}
	if (err == io.EOF || (err == nil && event.Type == EventDone)) && !s.logged {
		s.flushLocked()
	}
	s.mu.Unlock()

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
// as claude-bin's per-turn temp image files. MCP servers and other
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
		if s.cleanup != nil {
			s.closeOnce.Do(s.cleanup)
		}
	}
	return event, err
}

func (s *cleanupStream) Close() error {
	err := s.inner.Close()
	if s.cleanup != nil {
		s.closeOnce.Do(s.cleanup)
	}
	return err
}
