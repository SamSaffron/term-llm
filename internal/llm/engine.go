package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
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
	provider Provider
	tools    *ToolRegistry
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

// Stream returns a stream, applying external tools when needed.
func (e *Engine) Stream(ctx context.Context, req Request) (Stream, error) {
	if req.Search {
		caps := e.provider.Capabilities()
		needsExternalSearch := !caps.NativeWebSearch || req.ForceExternalSearch
		// Add external fetch if using external search and either:
		// 1. Provider lacks native fetch, OR
		// 2. We're forcing external search (--no-native-search)
		needsExternalFetch := needsExternalSearch && (!caps.NativeWebFetch || req.ForceExternalSearch)

		if needsExternalSearch || needsExternalFetch {
			return e.streamWithExternalTools(ctx, req, needsExternalSearch, needsExternalFetch)
		}
	}

	// If request has tools (e.g., MCP tools), use tool execution loop
	if len(req.Tools) > 0 && e.provider.Capabilities().ToolCalls {
		return e.streamWithToolExecution(ctx, req)
	}

	if req.DebugRaw {
		DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), req, "Request")
	}
	stream, err := e.provider.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	return WrapDebugStream(req.DebugRaw, stream), nil
}

func (e *Engine) applyExternalSearch(ctx context.Context, req Request, events chan<- Event) (Request, error) {
	if !e.provider.Capabilities().ToolCalls {
		return Request{}, fmt.Errorf("provider does not support tool calls for external search")
	}

	searchTool, ok := e.tools.Get(WebSearchToolName)
	if !ok {
		return Request{}, fmt.Errorf("web_search tool is not registered")
	}

	searchReq := req
	searchReq.Search = false
	searchReq.Tools = []ToolSpec{searchTool.Spec()}
	searchReq.ToolChoice = ToolChoice{Mode: ToolChoiceName, Name: WebSearchToolName}
	searchReq.ParallelToolCalls = false
	searchReq.DebugRaw = req.DebugRaw

	if searchReq.DebugRaw {
		DebugRawRequest(searchReq.DebugRaw, e.provider.Name(), e.provider.Credential(), searchReq, "Request (search tool call)")
	}

	stream, err := e.provider.Stream(ctx, searchReq)
	if err != nil {
		return Request{}, err
	}
	defer stream.Close()

	toolCalls, err := collectToolCalls(stream, req.DebugRaw)
	if err != nil {
		return Request{}, err
	}
	if len(toolCalls) == 0 {
		return Request{}, fmt.Errorf("search step returned no tool calls")
	}
	toolCalls = ensureToolCallIDs(toolCalls)

	for _, call := range toolCalls {
		DebugToolCall(req.Debug, call)
		if call.Name != WebSearchToolName {
			return Request{}, fmt.Errorf("unexpected tool call during search: %s", call.Name)
		}
	}

	// Notify that search is starting (after LLM returned tool call, before execution)
	if events != nil {
		events <- Event{Type: EventToolExecStart, ToolName: WebSearchToolName}
	}

	toolResults, err := e.executeToolCalls(ctx, toolCalls, req.Debug, req.DebugRaw)
	if err != nil {
		return Request{}, err
	}

	req.Messages = append(req.Messages, toolCallMessage(toolCalls))
	req.Messages = append(req.Messages, toolResults...)
	req.Search = false
	return req, nil
}

func collectToolCalls(stream Stream, debugRaw bool) ([]ToolCall, error) {
	var calls []ToolCall
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if event.Type == EventError && event.Err != nil {
			return nil, event.Err
		}
		if debugRaw {
			DebugRawEvent(true, event)
		}
		if event.Type == EventToolCall && event.Tool != nil {
			calls = append(calls, *event.Tool)
		}
	}
	return calls, nil
}

func (e *Engine) streamWithExternalTools(ctx context.Context, req Request, addSearch, addFetch bool) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		if req.DebugRaw {
			DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), req, "Request (initial)")
		}

		// Build list of external tools to add
		var externalTools []ToolSpec
		var externalToolNames []string
		if addSearch {
			if t, ok := e.tools.Get(WebSearchToolName); ok {
				externalTools = append(externalTools, t.Spec())
				externalToolNames = append(externalToolNames, WebSearchToolName)
			}
		}
		if addFetch {
			if t, ok := e.tools.Get(ReadURLToolName); ok {
				externalTools = append(externalTools, t.Spec())
				externalToolNames = append(externalToolNames, ReadURLToolName)
			}
		}

		debugMsg := fmt.Sprintf("adding external tools: %v", externalToolNames)
		debugSection(req.Debug, "External Tools", debugMsg)
		DebugRawSection(req.DebugRaw, "External Tools", debugMsg)

		// If we need external search, force an initial search call
		if addSearch {
			updated, err := e.applyExternalSearch(ctx, req, events)
			if err != nil {
				return err
			}
			req = updated

			// Back to thinking - LLM will process search results
			events <- Event{Type: EventToolExecStart, ToolName: ""}
		}

		// Add external tools for follow-up requests
		req.Tools = append(req.Tools, externalTools...)
		req.ToolChoice = ToolChoice{Mode: ToolChoiceAuto}

		if req.DebugRaw {
			label := "Request (with external tools)"
			if addSearch {
				label = "Request (with search results)"
			}
			DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), req, label)
		}

		maxTurns := getMaxTurns(req)
		for attempt := 0; attempt < maxTurns; attempt++ {
			if attempt == maxTurns-1 {
				req.Messages = append(req.Messages, SystemText(stopSearchToolHint))
			}

			stream, err := e.provider.Stream(ctx, req)
			if err != nil {
				return err
			}

			// Stream events in real-time, only collect tool calls
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
					continue // Don't forward tool calls yet
				}
				if event.Type == EventDone {
					continue // Don't forward done yet
				}
				// Forward all other events in real-time (text, tool exec progress, etc.)
				events <- event
			}
			stream.Close()

			// Split calls into our external tools vs other tools
			ourCalls, otherCalls := splitExternalToolCalls(toolCalls, externalToolNames)
			if len(ourCalls) == 0 {
				// No external tool calls - done
				events <- Event{Type: EventDone}
				return nil
			}

			if len(otherCalls) > 0 {
				return fmt.Errorf("mixed tool calls during external tool execution")
			}

			if attempt == maxTurns-1 {
				return fmt.Errorf("external tools exceeded max turns (%d)", maxTurns)
			}

			ourCalls = ensureToolCallIDs(ourCalls)
			for _, call := range ourCalls {
				DebugToolCall(req.Debug, call)
			}

			// Notify which tool is starting (for each call)
			for _, call := range ourCalls {
				info := extractToolInfo(call)
				events <- Event{Type: EventToolExecStart, ToolName: call.Name, ToolInfo: info}
			}

			toolResults, err := e.executeToolCalls(ctx, ourCalls, req.Debug, req.DebugRaw)
			if err != nil {
				return err
			}

			req.Messages = append(req.Messages, toolCallMessage(ourCalls))
			req.Messages = append(req.Messages, toolResults...)

			// Back to thinking - LLM will process results
			events <- Event{Type: EventToolExecStart, ToolName: ""}

			if req.DebugRaw {
				DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), req, fmt.Sprintf("Request (external tools loop %d)", attempt+1))
			}
		}

		return fmt.Errorf("external tools loop ended unexpectedly")
	}), nil
}

func (e *Engine) executeToolCalls(ctx context.Context, calls []ToolCall, debug bool, debugRaw bool) ([]Message, error) {
	results := make([]Message, 0, len(calls))
	for _, call := range calls {
		tool, ok := e.tools.Get(call.Name)
		if !ok {
			return nil, fmt.Errorf("tool not registered: %s", call.Name)
		}
		output, err := tool.Execute(ctx, call.Arguments)
		if err != nil {
			return nil, fmt.Errorf("tool %s failed: %w", call.Name, err)
		}
		DebugToolResult(debug, call.ID, call.Name, output)
		DebugRawToolResult(debugRaw, call.ID, call.Name, output)
		results = append(results, ToolResultMessage(call.ID, call.Name, output))
	}
	return results, nil
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

func splitExternalToolCalls(calls []ToolCall, externalToolNames []string) ([]ToolCall, []ToolCall) {
	var ourCalls []ToolCall
	var otherCalls []ToolCall
	for _, call := range calls {
		isExternal := false
		for _, name := range externalToolNames {
			if call.Name == name {
				isExternal = true
				break
			}
		}
		if isExternal {
			ourCalls = append(ourCalls, call)
		} else {
			otherCalls = append(otherCalls, call)
		}
	}
	return ourCalls, otherCalls
}

// extractToolInfo extracts display info from a tool call (e.g., URL for read_url)
func extractToolInfo(call ToolCall) string {
	if call.Name != ReadURLToolName {
		return ""
	}
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return ""
	}
	return args.URL
}

// streamWithToolExecution handles arbitrary tool calls (e.g., MCP tools).
func (e *Engine) streamWithToolExecution(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		if req.DebugRaw {
			DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), req, "Request (with tools)")
		}

		maxTurns := getMaxTurns(req)
		for attempt := 0; attempt < maxTurns; attempt++ {
			if attempt == maxTurns-1 {
				req.Messages = append(req.Messages, SystemText(stopSearchToolHint))
			}

			stream, err := e.provider.Stream(ctx, req)
			if err != nil {
				return err
			}

			// Stream events in real-time, collect tool calls
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
					continue // Don't forward tool calls yet
				}
				if event.Type == EventDone {
					continue // Don't forward done yet
				}
				// Forward all other events in real-time (text, etc.)
				events <- event
			}
			stream.Close()

			// If no tool calls, we're done
			if len(toolCalls) == 0 {
				events <- Event{Type: EventDone}
				return nil
			}

			toolCalls = ensureToolCallIDs(toolCalls)

			// Split into registered (executable) and unregistered (passthrough) tools
			var registeredCalls, unregisteredCalls []ToolCall
			for _, call := range toolCalls {
				if _, ok := e.tools.Get(call.Name); ok {
					registeredCalls = append(registeredCalls, call)
				} else {
					unregisteredCalls = append(unregisteredCalls, call)
				}
			}

			// Forward unregistered tool calls as events (e.g., suggest_commands)
			for i := range unregisteredCalls {
				call := unregisteredCalls[i]
				DebugToolCall(req.Debug, call)
				events <- Event{Type: EventToolCall, Tool: &call}
			}

			// If only unregistered calls, we're done
			if len(registeredCalls) == 0 {
				events <- Event{Type: EventDone}
				return nil
			}

			if attempt == maxTurns-1 {
				return fmt.Errorf("tool execution exceeded max turns (%d)", maxTurns)
			}

			for _, call := range registeredCalls {
				DebugToolCall(req.Debug, call)
			}

			// Notify which tool is starting (for each call)
			for _, call := range registeredCalls {
				events <- Event{Type: EventToolExecStart, ToolName: call.Name}
			}

			// Execute registered tool calls
			toolResults, err := e.executeToolCalls(ctx, registeredCalls, req.Debug, req.DebugRaw)
			if err != nil {
				return fmt.Errorf("tool execution: %w", err)
			}

			// Append tool call message and results to conversation (only registered calls)
			req.Messages = append(req.Messages, toolCallMessage(registeredCalls))
			req.Messages = append(req.Messages, toolResults...)

			// Back to thinking - LLM will process results
			events <- Event{Type: EventToolExecStart, ToolName: ""}

			if req.DebugRaw {
				DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), req, fmt.Sprintf("Request (tool loop %d)", attempt+1))
			}
		}

		return fmt.Errorf("tool execution loop ended unexpectedly")
	}), nil
}
