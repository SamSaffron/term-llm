package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// jsProvider adapts the browser's OpenAI-compatible inference function to the
// term-llm provider contract. It owns no weights and never fabricates output:
// every token and tool call comes from the JavaScript adapter.
type jsProvider struct {
	inference Inference
	model     string
}

func (p *jsProvider) Name() string       { return demoProviderName }
func (p *jsProvider) Credential() string { return "browser" }

func (p *jsProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: true, SupportsToolChoice: false}
}

func (p *jsProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	body, err := buildOpenAIRequest(req)
	if err != nil {
		return nil, err
	}
	stream := &providerStream{ctx: ctx, ch: make(chan core.Event, 64), inference: p.inference}
	go p.run(ctx, body, stream)
	return stream, nil
}

func (p *jsProvider) run(ctx context.Context, requestJSON string, stream *providerStream) {
	defer close(stream.ch)
	streamedText := false
	result, err := p.inference.Complete(ctx, requestJSON, func(chunkJSON string) {
		var chunk openAIStreamChunk
		if json.Unmarshal([]byte(chunkJSON), &chunk) != nil {
			return
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content == "" {
				continue
			}
			streamedText = true
			stream.send(core.Event{Type: core.EventTextDelta, Text: choice.Delta.Content})
		}
	})
	if err != nil {
		if ctx.Err() != nil {
			stream.send(core.Event{Type: core.EventError, Err: ctx.Err()})
			return
		}
		stream.send(core.Event{Type: core.EventError, Err: fmt.Errorf("browser inference: %w", err)})
		return
	}

	var response openAIResponse
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		stream.send(core.Event{Type: core.EventError, Err: fmt.Errorf("decode browser inference response: %w", err)})
		return
	}
	if len(response.Choices) == 0 {
		stream.send(core.Event{Type: core.EventError, Err: fmt.Errorf("browser inference returned no choices")})
		return
	}
	message := response.Choices[0].Message
	if !streamedText && message.Content != "" {
		stream.send(core.Event{Type: core.EventTextDelta, Text: message.Content})
	}
	for _, call := range message.ToolCalls {
		args := normalizeArguments(call.Function.Arguments)
		stream.send(core.Event{Type: core.EventToolCall, Tool: &core.ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: args,
		}})
	}
	if response.Usage != nil {
		stream.send(core.Event{Type: core.EventUsage, Use: &core.Usage{
			InputTokens:  response.Usage.PromptTokens,
			OutputTokens: response.Usage.CompletionTokens,
		}})
	}
	stream.send(core.Event{Type: core.EventDone})
}

type providerStream struct {
	ctx       context.Context
	ch        chan core.Event
	inference Inference
	closed    atomic.Bool
}

func (s *providerStream) send(event core.Event) {
	if s.closed.Load() {
		return
	}
	select {
	case s.ch <- event:
	case <-s.ctx.Done():
	}
}

func (s *providerStream) Recv() (core.Event, error) {
	event, ok := <-s.ch
	if !ok {
		return core.Event{}, io.EOF
	}
	return event, nil
}

func (s *providerStream) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		// Interrupt the browser generation if this stream is closed while it is
		// still running (user pressed stop, or the page is going away).
		s.inference.CancelInference()
	}
	return nil
}

// buildOpenAIRequest translates a term-llm request into the OpenAI wire shape
// the browser adapter consumes.
func buildOpenAIRequest(req core.Request) (string, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = demoModelName
	}
	body := map[string]any{
		"model":    model,
		"messages": wireMessages(req.Messages),
		"stream":   true,
	}
	if req.MaxOutputTokens > 0 {
		body["max_tokens"] = req.MaxOutputTokens
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, spec := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        spec.Name,
					"description": spec.Description,
					"parameters":  spec.Schema,
				},
			})
		}
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("encode browser inference request: %w", err)
	}
	return string(encoded), nil
}

func wireMessages(messages []core.Message) []map[string]any {
	result := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case core.RoleSystem, core.RoleDeveloper, core.RoleUser:
			role := "system"
			if message.Role == core.RoleUser {
				role = "user"
			}
			result = append(result, map[string]any{"role": role, "content": textParts(message.Parts)})
		case core.RoleAssistant:
			text, calls := assistantParts(message.Parts)
			entry := map[string]any{"role": "assistant", "content": text}
			if len(calls) > 0 {
				entry["tool_calls"] = calls
			}
			result = append(result, entry)
		case core.RoleTool:
			for _, part := range message.Parts {
				if part.Type != core.PartToolResult || part.ToolResult == nil {
					continue
				}
				result = append(result, map[string]any{
					"role":         "tool",
					"tool_call_id": part.ToolResult.ID,
					"content":      part.ToolResult.Content,
				})
			}
		}
	}
	return result
}

func textParts(parts []core.Part) string {
	var texts []string
	for _, part := range parts {
		if (part.Type == core.PartText || part.Type == "") && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func assistantParts(parts []core.Part) (string, []map[string]any) {
	var texts []string
	var calls []map[string]any
	for _, part := range parts {
		switch part.Type {
		case core.PartText:
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		case core.PartToolCall:
			if part.ToolCall == nil {
				continue
			}
			args := string(part.ToolCall.Arguments)
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id": part.ToolCall.ID, "type": "function",
				"function": map[string]any{"name": part.ToolCall.Name, "arguments": args},
			})
		}
	}
	return strings.Join(texts, "\n"), calls
}

type openAIResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

// normalizeArguments accepts either a JSON string (the OpenAI shape) or an
// already-decoded JSON value, and always returns valid object JSON.
func normalizeArguments(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage("{}")
	}
	if strings.HasPrefix(trimmed, `"`) {
		var inner string
		if json.Unmarshal(raw, &inner) == nil && strings.TrimSpace(inner) != "" {
			return json.RawMessage(inner)
		}
	}
	var probe any
	if json.Unmarshal(raw, &probe) == nil {
		return raw
	}
	return json.RawMessage("{}")
}
