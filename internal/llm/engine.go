package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/usage"
)

const (
	defaultMaxTurns    = 20
	stopSearchToolHint = "IMPORTANT: Do not call any tools. Use the information already retrieved and answer directly."
)

// getMaxTurns returns the max turns from request, with fallback to default
func getMaxTurns(req Request) int {
	if req.MaxTurns > 0 {
		return req.MaxTurns
	}
	return defaultMaxTurns
}

// Engine orchestrates provider calls and external tool execution.
type Engine struct {
	provider    Provider
	tools       *ToolRegistry
	debugLogger *DebugLogger
}

func NewEngine(provider Provider, tools *ToolRegistry) *Engine {
	if tools == nil {
		tools = NewToolRegistry()
	}
	return &Engine{
		provider: provider,
		tools:    tools,
	}
}

// RegisterTool adds a tool to the engine's registry.
func (e *Engine) RegisterTool(tool Tool) {
	e.tools.Register(tool)
}

// UnregisterTool removes a tool from the engine's registry.
func (e *Engine) UnregisterTool(name string) {
	e.tools.Unregister(name)
}

// Tools returns the engine's tool registry.
func (e *Engine) Tools() *ToolRegistry {
	return e.tools
}

// SetDebugLogger sets the debug logger for this engine.
func (e *Engine) SetDebugLogger(logger *DebugLogger) {
	e.debugLogger = logger
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
		needsExternalFetch := !caps.NativeWebFetch || req.ForceExternalSearch

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

	// 2. Decide if we use the agentic loop
	// We use it if request has tools AND provider supports tool calls
	useLoop := len(req.Tools) > 0 && caps.ToolCalls

	if useLoop {
		stream := newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
			return e.runLoop(ctx, req, events)
		})
		stream = wrapLoggingStream(stream, e.provider.Name(), req.Model)
		return e.wrapDebugLoggingStream(stream), nil
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
	return e.wrapDebugLoggingStream(stream), nil
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

func (e *Engine) runLoop(ctx context.Context, req Request, events chan<- Event) error {
	maxTurns := getMaxTurns(req)
	originalToolChoice := req.ToolChoice
	restoredToolChoice := false

	for attempt := 0; attempt < maxTurns; attempt++ {
		// Prepare turn
		if attempt == maxTurns-1 {
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
			return err
		}

		// Collect tool calls, forward all other events
		var toolCalls []ToolCall
		for {
			event, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				stream.Close()
				return err
			}
			if event.Type == EventError && event.Err != nil {
				stream.Close()
				return event.Err
			}
			if req.DebugRaw {
				DebugRawEvent(true, event)
			}
			if event.Type == EventToolCall && event.Tool != nil {
				toolCalls = append(toolCalls, *event.Tool)
				continue
			}
			if event.Type == EventDone {
				continue
			}
			events <- event
		}
		stream.Close()

		// Search is only performed once (either pre-emptively or in first turn)
		req.Search = false

		if len(toolCalls) == 0 {
			// No tools called - check if we should restore original tool choice and retry once
			if originalToolChoice.Mode == ToolChoiceName && !restoredToolChoice {
				req.ToolChoice = originalToolChoice
				restoredToolChoice = true
				continue
			}
			events <- Event{Type: EventDone}
			return nil
		}

		toolCalls = ensureToolCallIDs(toolCalls)

		// Split into registered (to execute) and unregistered (to passthrough)
		var registered, unregistered []ToolCall
		for _, call := range toolCalls {
			if _, ok := e.tools.Get(call.Name); ok {
				registered = append(registered, call)
			} else {
				unregistered = append(unregistered, call)
			}
		}

		// Forward unregistered tool calls as events
		for i := range unregistered {
			call := unregistered[i]
			DebugToolCall(req.Debug, call)
			events <- Event{Type: EventToolCall, Tool: &call}
		}

		// If nothing to execute, we are done
		if len(registered) == 0 {
			events <- Event{Type: EventDone}
			return nil
		}

		if attempt == maxTurns-1 {
			return fmt.Errorf("agentic loop exceeded max turns (%d)", maxTurns)
		}

		// Execute registered tools
		for _, call := range registered {
			DebugToolCall(req.Debug, call)
			info := e.getToolPreview(call)

			if events != nil {
				events <- Event{Type: EventToolExecStart, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: info}
			}
		}

		toolResults, err := e.executeToolCalls(ctx, registered, events, req.Debug, req.DebugRaw)
		if err != nil {
			return err
		}

		req.Messages = append(req.Messages, toolCallMessage(registered))
		req.Messages = append(req.Messages, toolResults...)
	}

	return fmt.Errorf("agentic loop ended unexpectedly")
}

// executeToolCalls executes multiple tool calls, potentially in parallel.
// Note: When executing in parallel, EventToolExecStart/EventToolExecEnd events
// are emitted from concurrent goroutines. While the channel is thread-safe, events
// may arrive in non-deterministic order. Consumers should use ToolCallID to correlate
// start/end events rather than relying on ordering.
func (e *Engine) executeToolCalls(ctx context.Context, calls []ToolCall, events chan<- Event, debug bool, debugRaw bool) ([]Message, error) {
	// Fast path: single call, no concurrency overhead
	if len(calls) == 1 {
		return e.executeSingleToolCall(ctx, calls[0], events, debug, debugRaw)
	}

	// Parallel execution for multiple calls (events may arrive out of order)
	type toolResult struct {
		index   int
		message Message
	}

	var wg sync.WaitGroup
	resultChan := make(chan toolResult, len(calls))

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c ToolCall) {
			defer wg.Done()
			msgs, _ := e.executeSingleToolCall(ctx, c, events, debug, debugRaw)
			if len(msgs) > 0 {
				resultChan <- toolResult{index: idx, message: msgs[0]}
			}
		}(i, call)
	}

	// Close channel when all goroutines complete
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect results and maintain original order
	results := make([]Message, len(calls))
	for r := range resultChan {
		results[r.index] = r.message
	}

	return results, nil
}

// executeSingleToolCall executes a single tool call and returns the result message.
func (e *Engine) executeSingleToolCall(ctx context.Context, call ToolCall, events chan<- Event, debug bool, debugRaw bool) ([]Message, error) {
	tool, ok := e.tools.Get(call.Name)
	if !ok {
		errMsg := fmt.Sprintf("Error: tool not registered: %s", call.Name)
		DebugToolResult(debug, call.ID, call.Name, errMsg)
		if events != nil {
			events <- Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: e.getToolPreview(call), ToolSuccess: false}
		}
		return []Message{ToolErrorMessage(call.ID, call.Name, errMsg, call.ThoughtSig)}, nil
	}

	output, err := tool.Execute(ctx, call.Arguments)
	info := e.getToolPreview(call)

	if err != nil {
		errMsg := fmt.Sprintf("Error: %v", err)
		DebugToolResult(debug, call.ID, call.Name, errMsg)
		if events != nil {
			events <- Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: info, ToolSuccess: false}
		}
		return []Message{ToolErrorMessage(call.ID, call.Name, errMsg, call.ThoughtSig)}, nil
	}

	DebugToolResult(debug, call.ID, call.Name, output)
	DebugRawToolResult(debugRaw, call.ID, call.Name, output)
	if events != nil {
		events <- Event{Type: EventToolExecEnd, ToolCallID: call.ID, ToolName: call.Name, ToolInfo: info, ToolSuccess: true}
	}
	return []Message{ToolResultMessage(call.ID, call.Name, output, call.ThoughtSig)}, nil
}

func toolCallMessage(calls []ToolCall) Message {
	parts := make([]Part, 0, len(calls))
	for i := range calls {
		call := calls[i]
		parts = append(parts, Part{
			Type:     PartToolCall,
			ToolCall: &call,
		})
	}
	return Message{
		Role:  RoleAssistant,
		Parts: parts,
	}
}

func ensureToolCallIDs(calls []ToolCall) []ToolCall {
	for i := range calls {
		if strings.TrimSpace(calls[i].ID) == "" {
			calls[i].ID = fmt.Sprintf("toolcall-%d", i+1)
		}
	}
	return calls
}

// getToolPreview returns a preview string for a tool call.
func (e *Engine) getToolPreview(call ToolCall) string {
	if tool, ok := e.tools.Get(call.Name); ok {
		if preview := tool.Preview(call.Arguments); preview != "" {
			if !strings.HasPrefix(preview, "(") {
				return "(" + preview + ")"
			}
			return preview
		}
	}
	return extractToolInfo(call)
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

func extractToolInfo(call ToolCall) string {
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

	// Accumulated usage (multiple EventUsage events in agentic loops)
	totalInput      int
	totalOutput     int
	totalCacheRead  int
	totalCacheWrite int
	logged          bool // Prevent double-logging
}

func (s *loggingStream) Recv() (Event, error) {
	event, err := s.inner.Recv()

	// Accumulate usage from each EventUsage
	if err == nil && event.Type == EventUsage && event.Use != nil {
		s.totalInput += event.Use.InputTokens
		s.totalOutput += event.Use.OutputTokens
		s.totalCacheRead += event.Use.CachedInputTokens
	}

	// Log on EOF (stream complete) or EventDone
	if (err == io.EOF || (err == nil && event.Type == EventDone)) && !s.logged {
		s.flush()
	}

	return event, err
}

func (s *loggingStream) Close() error {
	// Also flush on explicit close (in case EOF wasn't received)
	if !s.logged {
		s.flush()
	}
	return s.inner.Close()
}

func (s *loggingStream) flush() {
	if s.totalInput == 0 && s.totalOutput == 0 {
		return // Nothing to log
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
