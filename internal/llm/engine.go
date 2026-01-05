package llm

import (
	"context"
	"fmt"
	"io"
	"strings"
)

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

// Stream returns a stream, applying external search when needed.
func (e *Engine) Stream(ctx context.Context, req Request) (Stream, error) {
	if req.DebugRaw {
		DebugRawRequest(req.DebugRaw, e.provider.Name(), req, "Request (initial)")
	}
	if req.Search && !e.provider.Capabilities().NativeSearch {
		debugSection(req.Debug, "External Search", "provider lacks native search; using web_search tool")
		DebugRawSection(req.DebugRaw, "External Search", "provider lacks native search; using web_search tool")
		updated, err := e.applyExternalSearch(ctx, req)
		if err != nil {
			return nil, err
		}
		req = updated
		if req.DebugRaw {
			DebugRawRequest(req.DebugRaw, e.provider.Name(), req, "Request (with search results)")
		}
	}
	stream, err := e.provider.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	return WrapDebugStream(req.DebugRaw, stream), nil
}

func (e *Engine) applyExternalSearch(ctx context.Context, req Request) (Request, error) {
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

	stream, err := e.provider.Stream(ctx, searchReq)
	if err != nil {
		return Request{}, err
	}
	defer stream.Close()

	toolCalls, err := collectToolCalls(stream)
	if err != nil {
		return Request{}, err
	}
	if len(toolCalls) == 0 {
		return Request{}, fmt.Errorf("search step returned no tool calls")
	}
	toolCalls = ensureToolCallIDs(toolCalls)

	for _, call := range toolCalls {
		DebugRawToolCall(req.DebugRaw, call)
		DebugToolCall(req.Debug, call)
		if call.Name != WebSearchToolName {
			return Request{}, fmt.Errorf("unexpected tool call during search: %s", call.Name)
		}
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

func collectToolCalls(stream Stream) ([]ToolCall, error) {
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
		if event.Type == EventToolCall && event.Tool != nil {
			calls = append(calls, *event.Tool)
		}
	}
	return calls, nil
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
