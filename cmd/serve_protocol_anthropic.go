package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

type anthropicMessagesRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Messages    []anthropicMessage `json:"messages"`
	System      json.RawMessage    `json:"system,omitempty"`
	Tools       []anthropicToolDef `json:"tools,omitempty"`
	ToolChoice  json.RawMessage    `json:"tool_choice,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	Temperature *float32           `json:"temperature,omitempty"`
	TopP        *float32           `json:"top_p,omitempty"`
	Metadata    json.RawMessage    `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source,omitempty"`
}

type anthropicToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

func writeAnthropicSSE(w io.Writer, eventType string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b)
	return err
}

// parseAnthropicMessages converts Anthropic-format messages to llm.Message.
func parseAnthropicMessages(msgs []anthropicMessage) ([]llm.Message, error) {
	result := make([]llm.Message, 0, len(msgs))

	for _, msg := range msgs {
		role := strings.ToLower(strings.TrimSpace(msg.Role))

		// Try string shorthand first
		var textContent string
		if err := json.Unmarshal(msg.Content, &textContent); err == nil {
			switch role {
			case "user":
				result = append(result, llm.UserText(textContent))
			case "assistant":
				result = append(result, llm.AssistantText(textContent))
			default:
				return nil, fmt.Errorf("unsupported message role: %s", role)
			}
			continue
		}

		// Array of content blocks
		var blocks []anthropicContentBlock
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			return nil, fmt.Errorf("invalid content for %s message", role)
		}

		var parts []llm.Part
		for _, block := range blocks {
			switch block.Type {
			case "text":
				if block.Text != "" {
					parts = append(parts, llm.Part{Type: llm.PartText, Text: block.Text})
				}
			case "image":
				if block.Source != nil {
					parts = append(parts, llm.Part{
						Type:      llm.PartImage,
						ImageData: &llm.ToolImageData{MediaType: block.Source.MediaType, Base64: block.Source.Data},
					})
				}
			case "tool_use":
				args := block.Input
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				parts = append(parts, llm.Part{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:        block.ID,
						Name:      block.Name,
						Arguments: args,
					},
				})
			case "tool_result":
				content := ""
				// tool_result content can be string or array of blocks
				if len(block.Content) > 0 {
					var s string
					if err := json.Unmarshal(block.Content, &s); err == nil {
						content = s
					} else {
						var inner []anthropicContentBlock
						if err := json.Unmarshal(block.Content, &inner); err == nil {
							var b strings.Builder
							for _, ib := range inner {
								if ib.Type == "text" {
									b.WriteString(ib.Text)
								}
							}
							content = b.String()
						}
					}
				}
				parts = append(parts, llm.Part{
					Type: llm.PartToolResult,
					ToolResult: &llm.ToolResult{
						ID:      block.ToolUseID,
						Content: content,
						IsError: block.IsError,
					},
				})
			}
		}

		if len(parts) == 0 {
			continue
		}

		switch role {
		case "user":
			// Anthropic puts tool_result blocks in user messages.
			// Split them out into RoleTool messages so the OpenAI-compat
			// provider can emit proper {"role":"tool"} entries.
			var userParts, toolParts []llm.Part
			for _, p := range parts {
				if p.Type == llm.PartToolResult {
					toolParts = append(toolParts, p)
				} else {
					userParts = append(userParts, p)
				}
			}
			if len(toolParts) > 0 {
				result = append(result, llm.Message{Role: llm.RoleTool, Parts: toolParts})
			}
			if len(userParts) > 0 {
				result = append(result, llm.Message{Role: llm.RoleUser, Parts: userParts})
			}
		case "assistant":
			result = append(result, llm.Message{Role: llm.RoleAssistant, Parts: parts})
		default:
			return nil, fmt.Errorf("unsupported message role: %s", role)
		}
	}

	return result, nil
}

// parseAnthropicSystem extracts system text from the system field (string or array).
func parseAnthropicSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Type == "text" {
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	return ""
}

// anthropicToolsToSpecs converts Anthropic tool definitions to llm.ToolSpec.
// This allows clients (e.g. Claude Code) to pass their own tool definitions
// through to the backend model.
func anthropicToolsToSpecs(tools []anthropicToolDef) []llm.ToolSpec {
	specs := make([]llm.ToolSpec, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		specs = append(specs, llm.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.InputSchema,
		})
	}
	return specs
}

func parseAnthropicRequestedToolNames(tools []anthropicToolDef) map[string]bool {
	requested := make(map[string]bool, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		requested[name] = true
	}
	return requested
}

// parseAnthropicToolChoice parses Anthropic-format tool_choice into llm.ToolChoice.
func parseAnthropicToolChoice(raw json.RawMessage) llm.ToolChoice {
	if len(raw) == 0 {
		return llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}
	switch obj.Type {
	case "any":
		return llm.ToolChoice{Mode: llm.ToolChoiceRequired}
	case "tool":
		return llm.ToolChoice{Mode: llm.ToolChoiceName, Name: obj.Name}
	case "none":
		return llm.ToolChoice{Mode: llm.ToolChoiceNone}
	default:
		return llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	}
}

// anthropicMessagesFinalResponse builds the non-streaming Anthropic response body.
func anthropicMessagesFinalResponse(result serveRunResult, model string) map[string]any {
	content := []map[string]any{}
	if result.Text.Len() > 0 {
		content = append(content, map[string]any{
			"type": "text",
			"text": result.Text.String(),
		})
	}
	for _, call := range result.ToolCalls {
		var input any
		if err := json.Unmarshal(call.Arguments, &input); err != nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Name,
			"input": input,
		})
	}
	stopReason := "end_turn"
	if len(result.ToolCalls) > 0 {
		stopReason = "tool_use"
	}
	return map[string]any{
		"id":            "msg_" + randomSuffix(),
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         model,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                result.Usage.InputTokens,
			"output_tokens":               result.Usage.OutputTokens,
			"cache_creation_input_tokens": result.Usage.CacheWriteTokens,
			"cache_read_input_tokens":     result.Usage.CachedInputTokens,
		},
	}
}
