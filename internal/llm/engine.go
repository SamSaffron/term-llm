package llm

import (
	"context"
	"fmt"
	"io"
	"strings"
)

const (
	maxExternalSearchLoops = 6
	stopSearchToolHint     = "IMPORTANT: Do not call any tools. Use the information already retrieved and answer directly."
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
	if req.Search && !e.provider.Capabilities().NativeSearch {
		return e.streamWithExternalSearch(ctx, req)
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

func (e *Engine) streamWithExternalSearch(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		if req.DebugRaw {
			DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), req, "Request (initial)")
		}
		debugSection(req.Debug, "External Search", "provider lacks native search; using web_search tool")
		DebugRawSection(req.DebugRaw, "External Search", "provider lacks native search; using web_search tool")

		updated, err := e.applyExternalSearch(ctx, req)
		if err != nil {
			return err
		}
		req = updated
		if req.DebugRaw {
			DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), req, "Request (with search results)")
		}

		for attempt := 0; attempt < maxExternalSearchLoops; attempt++ {
			if attempt == maxExternalSearchLoops-1 {
				req.Messages = append(req.Messages, SystemText(stopSearchToolHint))
			}

			stream, err := e.provider.Stream(ctx, req)
			if err != nil {
				return err
			}

			var buffered []Event
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
				}
				buffered = append(buffered, event)
			}
			stream.Close()

			searchCalls, otherCalls := splitSearchCalls(toolCalls)
			if len(searchCalls) == 0 {
				for _, event := range buffered {
					if event.Type == EventDone {
						continue
					}
					events <- event
				}
				events <- Event{Type: EventDone}
				return nil
			}

			if len(otherCalls) > 0 {
				return fmt.Errorf("mixed tool calls during external search")
			}

			if attempt == maxExternalSearchLoops-1 {
				return fmt.Errorf("external search exceeded max tool call loops (%d)", maxExternalSearchLoops)
			}

			searchCalls = ensureToolCallIDs(searchCalls)
			for _, call := range searchCalls {
				DebugToolCall(req.Debug, call)
			}

			toolResults, err := e.executeToolCalls(ctx, searchCalls, req.Debug, req.DebugRaw)
			if err != nil {
				return err
			}

			req.Messages = append(req.Messages, toolCallMessage(searchCalls))
			req.Messages = append(req.Messages, toolResults...)

			if req.DebugRaw {
				DebugRawRequest(req.DebugRaw, e.provider.Name(), e.provider.Credential(), req, fmt.Sprintf("Request (search loop %d)", attempt+1))
			}
		}

		return fmt.Errorf("external search loop ended unexpectedly")
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

func splitSearchCalls(calls []ToolCall) ([]ToolCall, []ToolCall) {
	var searchCalls []ToolCall
	var otherCalls []ToolCall
	for _, call := range calls {
		if call.Name == WebSearchToolName {
			searchCalls = append(searchCalls, call)
		} else {
			otherCalls = append(otherCalls, call)
		}
	}
	return searchCalls, otherCalls
}
