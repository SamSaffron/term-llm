package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

type chatCompletionsRequest struct {
	Model             string             `json:"model"`
	Messages          []chatMessage      `json:"messages"`
	Tools             []chatTool         `json:"tools,omitempty"`
	ToolChoice        json.RawMessage    `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls,omitempty"`
	Temperature       *float32           `json:"temperature,omitempty"`
	TopP              *float32           `json:"top_p,omitempty"`
	MaxTokens         int                `json:"max_tokens,omitempty"`
	Stream            bool               `json:"stream,omitempty"`
	StreamOptions     *chatStreamOptions `json:"stream_options,omitempty"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type chatTool struct {
	Type     string           `json:"type"`
	Name     string           `json:"name,omitempty"`
	Function *chatToolFuncDef `json:"function,omitempty"`
}

type chatToolFuncDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func parseChatMessages(msgs []chatMessage) ([]core.Message, bool, error) {
	callNameByID := make(map[string]string)
	result := make([]core.Message, 0, len(msgs))
	allToolMessages := len(msgs) > 0
	for _, msg := range msgs {
		if strings.ToLower(strings.TrimSpace(msg.Role)) != "tool" {
			allToolMessages = false
			break
		}
	}
	replaceHistory := len(msgs) > 1 && !allToolMessages

	for _, msg := range msgs {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system":
			result = append(result, core.SystemText(extractMessageText(msg.Content)))
			replaceHistory = true
		case "developer":
			result = append(result, core.Message{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: extractMessageText(msg.Content)}}})
			replaceHistory = true
		case "user":
			userMsg, err := parseUserMessageContent(msg.Content)
			if err != nil {
				return nil, false, fmt.Errorf("user message: %w", err)
			}
			result = append(result, userMsg)
		case "assistant":
			assistantMsg, err := parseUserMessageContent(msg.Content)
			if err != nil {
				return nil, false, fmt.Errorf("assistant message: %w", err)
			}
			parts := append([]core.Part{}, assistantMsg.Parts...)
			for _, tc := range msg.ToolCalls {
				args := tc.Function.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				callNameByID[tc.ID] = tc.Function.Name
				parts = append(parts, core.Part{
					Type: core.PartToolCall,
					ToolCall: &core.ToolCall{
						ID:        tc.ID,
						Name:      tc.Function.Name,
						Arguments: json.RawMessage(args),
					},
				})
			}
			if len(parts) == 0 {
				continue
			}
			result = append(result, core.Message{Role: core.RoleAssistant, Parts: parts})
			replaceHistory = true
		case "tool":
			callID := strings.TrimSpace(msg.ToolCallID)
			if callID == "" {
				return nil, false, fmt.Errorf("tool message missing tool_call_id")
			}
			name := strings.TrimSpace(msg.Name)
			if name == "" {
				name = callNameByID[callID]
			}
			result = append(result, core.ToolResultMessage(callID, name, extractMessageText(msg.Content), nil))
		default:
			return nil, false, fmt.Errorf("unsupported message role: %s", msg.Role)
		}
	}

	return result, replaceHistory, nil
}

func populateMissingToolResultNames(messages []core.Message, history []core.Message) {
	callNameByID := make(map[string]string)
	collectToolNamesByID(callNameByID, history)
	if len(callNameByID) == 0 {
		return
	}
	for i := range messages {
		for j := range messages[i].Parts {
			part := &messages[i].Parts[j]
			if part.Type != core.PartToolResult || part.ToolResult == nil {
				continue
			}
			if strings.TrimSpace(part.ToolResult.Name) != "" {
				continue
			}
			part.ToolResult.Name = callNameByID[strings.TrimSpace(part.ToolResult.ID)]
		}
	}
}

func collectToolNamesByID(callNameByID map[string]string, messages []core.Message) {
	for _, msg := range messages {
		for _, part := range msg.Parts {
			switch part.Type {
			case core.PartToolCall:
				if part.ToolCall == nil {
					continue
				}
				callID := strings.TrimSpace(part.ToolCall.ID)
				name := strings.TrimSpace(part.ToolCall.Name)
				if callID != "" && name != "" {
					callNameByID[callID] = name
				}
			case core.PartToolResult:
				if part.ToolResult == nil {
					continue
				}
				callID := strings.TrimSpace(part.ToolResult.ID)
				name := strings.TrimSpace(part.ToolResult.Name)
				if callID != "" && name != "" {
					callNameByID[callID] = name
				}
			}
		}
	}
}

func parseChatRequestedToolNames(tools []chatTool) map[string]bool {
	selected := map[string]bool{}
	for _, t := range tools {
		if strings.ToLower(t.Type) != "function" {
			continue
		}
		name := strings.TrimSpace(t.Name)
		if name == "" && t.Function != nil {
			name = strings.TrimSpace(t.Function.Name)
		}
		if name != "" {
			selected[name] = true
		}
	}
	return selected
}

func chatToolsToSpecs(tools []chatTool) []core.ToolSpec {
	specs := make([]core.ToolSpec, 0, len(tools))
	for _, t := range tools {
		if strings.ToLower(strings.TrimSpace(t.Type)) != "function" {
			continue
		}
		name := strings.TrimSpace(t.Name)
		spec := core.ToolSpec{}
		if t.Function != nil {
			if name == "" {
				name = strings.TrimSpace(t.Function.Name)
			}
			spec.Description = t.Function.Description
			spec.Schema = t.Function.Parameters
		}
		if name == "" {
			continue
		}
		spec.Name = name
		specs = append(specs, spec)
	}
	return specs
}

func parseToolChoice(raw json.RawMessage) core.ToolChoice {
	if len(raw) == 0 {
		return core.ToolChoice{Mode: core.ToolChoiceAuto}
	}
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return core.ToolChoice{Mode: core.ToolChoiceAuto}
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "none":
			return core.ToolChoice{Mode: core.ToolChoiceNone}
		case "required":
			return core.ToolChoice{Mode: core.ToolChoiceRequired}
		default:
			return core.ToolChoice{Mode: core.ToolChoiceAuto}
		}
	}

	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if strings.ToLower(strings.TrimSpace(obj.Type)) == "function" {
			name := strings.TrimSpace(obj.Name)
			if name == "" {
				name = strings.TrimSpace(obj.Function.Name)
			}
			if name != "" {
				return core.ToolChoice{Mode: core.ToolChoiceName, Name: name}
			}
		}
	}
	return core.ToolChoice{Mode: core.ToolChoiceAuto}
}

func chatCompletionFinalResponse(result serveRunResult, model string) map[string]any {
	message := map[string]any{
		"role":    "assistant",
		"content": result.Text.String(),
	}
	finishReason := "stop"
	if len(result.ToolCalls) > 0 {
		finishReason = "tool_calls"
		toolCalls := make([]map[string]any, 0, len(result.ToolCalls))
		for _, call := range result.ToolCalls {
			toolCalls = append(toolCalls, map[string]any{
				"id":   call.ID,
				"type": "function",
				"function": map[string]any{
					"name":      call.Name,
					"arguments": string(call.Arguments),
				},
			})
		}
		message["tool_calls"] = toolCalls
	}

	promptTokens := result.Usage.InputTokens + result.Usage.CachedInputTokens
	totalTokens := promptTokens + result.Usage.OutputTokens

	return map[string]any{
		"id":      "chatcmpl_" + randomSuffix(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": result.Usage.OutputTokens,
			"total_tokens":      totalTokens,
			"prompt_tokens_details": map[string]any{
				"cached_tokens":      result.Usage.CachedInputTokens,
				"cache_write_tokens": result.Usage.CacheWriteTokens,
			},
		},
	}
}
