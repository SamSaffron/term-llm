package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

type responsesCreateRequest struct {
	Model              string            `json:"model"`
	Provider           string            `json:"provider"`
	Input              json.RawMessage   `json:"input"`
	Tools              []json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens    int               `json:"max_output_tokens,omitempty"`
	Temperature        *float32          `json:"temperature,omitempty"`
	TopP               *float32          `json:"top_p,omitempty"`
	Stream             bool              `json:"stream,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	ReasoningEffort    string            `json:"reasoning_effort,omitempty"`
}

func parseResponsesInput(input json.RawMessage) ([]llm.Message, bool, error) {
	trimmed := strings.TrimSpace(string(input))
	if trimmed == "" || trimmed == "null" {
		return nil, false, fmt.Errorf("input is required")
	}

	// string shorthand
	var inputText string
	if err := json.Unmarshal(input, &inputText); err == nil {
		if strings.TrimSpace(inputText) == "" {
			return nil, false, fmt.Errorf("input is empty")
		}
		return []llm.Message{llm.UserText(inputText)}, false, nil
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, false, fmt.Errorf("invalid input format")
	}

	var messages []llm.Message
	callNameByID := map[string]string{}
	replaceHistory := false
	userCount := 0

	for _, item := range items {
		itemType := jsonString(item["type"])
		switch itemType {
		case "message":
			role := strings.ToLower(strings.TrimSpace(jsonString(item["role"])))
			switch role {
			case "developer":
				messages = append(messages, llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: extractItemContent(item["content"])}}})
				replaceHistory = true
			case "system":
				messages = append(messages, llm.SystemText(extractItemContent(item["content"])))
				replaceHistory = true
			case "assistant":
				messages = append(messages, llm.AssistantText(extractItemContent(item["content"])))
				replaceHistory = true
			default:
				msg, err := parseUserMessageContent(item["content"])
				if err != nil {
					return nil, false, fmt.Errorf("user message: %w", err)
				}
				messages = append(messages, msg)
				userCount++
			}
		case "function_call":
			id := jsonString(item["call_id"])
			name := jsonString(item["name"])
			args := jsonString(item["arguments"])
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			callNameByID[id] = name
			messages = append(messages, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
				Type:     llm.PartToolCall,
				ToolCall: &llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)},
			}}})
			replaceHistory = true
		case "function_call_output":
			id := jsonString(item["call_id"])
			out := jsonString(item["output"])
			messages = append(messages, llm.ToolResultMessage(id, callNameByID[id], out, nil))
			replaceHistory = true
		}
	}

	if userCount > 1 {
		replaceHistory = true
	}
	return messages, replaceHistory, nil
}

// normalizeReasoningEffort trims whitespace and folds the literal "default"
// value (sent by older clients and stale localStorage entries) to an empty
// string so that providers receive "" meaning "use the provider default"
// rather than a bogus "default" value that would produce an upstream 400.
func normalizeReasoningEffort(value string) string {
	v := strings.TrimSpace(value)
	if strings.EqualFold(v, "default") {
		return ""
	}
	return v
}

func parseRequestedTools(raw []json.RawMessage) (bool, map[string]bool, []llm.ToolSpec) {
	search := false
	toolNames := map[string]bool{}
	passthrough := make([]llm.ToolSpec, 0, len(raw))

	for _, item := range raw {
		var generic map[string]json.RawMessage
		if err := json.Unmarshal(item, &generic); err != nil {
			continue
		}
		typeName := strings.ToLower(strings.TrimSpace(jsonString(generic["type"])))
		switch typeName {
		case "web_search_preview", "web_search":
			search = true
		case "function":
			spec, ok := parseRequestedFunctionTool(generic)
			if !ok {
				continue
			}
			toolNames[spec.Name] = true
			passthrough = append(passthrough, spec)
		}
	}

	return search, toolNames, passthrough
}

func parseRequestedFunctionTool(generic map[string]json.RawMessage) (llm.ToolSpec, bool) {
	spec := llm.ToolSpec{
		Name:        strings.TrimSpace(jsonString(generic["name"])),
		Description: strings.TrimSpace(jsonString(generic["description"])),
	}
	if rawParams := generic["parameters"]; len(rawParams) > 0 {
		_ = json.Unmarshal(rawParams, &spec.Schema)
	}
	if rawStrict := generic["strict"]; len(rawStrict) > 0 {
		var strict bool
		if err := json.Unmarshal(rawStrict, &strict); err == nil && !strict {
			spec.NoStrict = true
		}
	}

	if rawFunc := generic["function"]; len(rawFunc) > 0 {
		var fn struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			Parameters  map[string]interface{} `json:"parameters"`
			Strict      *bool                  `json:"strict"`
		}
		if err := json.Unmarshal(rawFunc, &fn); err == nil {
			if spec.Name == "" {
				spec.Name = strings.TrimSpace(fn.Name)
			}
			if spec.Description == "" {
				spec.Description = strings.TrimSpace(fn.Description)
			}
			if spec.Schema == nil {
				spec.Schema = fn.Parameters
			}
			if fn.Strict != nil && !*fn.Strict {
				spec.NoStrict = true
			}
		}
	}

	if spec.Name == "" {
		return llm.ToolSpec{}, false
	}
	if spec.Schema == nil {
		spec.Schema = map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		}
	}
	return spec, true
}

func responsesFinalResponse(result serveRunResult, model string, respID string) map[string]any {
	output := []map[string]any{}
	if result.Text.Len() > 0 {
		output = append(output, map[string]any{
			"id":   "msg_" + randomSuffix(),
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{{
				"type": "output_text",
				"text": result.Text.String(),
			}},
		})
	}
	for _, call := range result.ToolCalls {
		output = append(output, map[string]any{
			"id":        "fc_" + call.ID,
			"type":      "function_call",
			"call_id":   call.ID,
			"name":      call.Name,
			"arguments": string(call.Arguments),
		})
	}

	return map[string]any{
		"id":      respID,
		"object":  "response",
		"created": time.Now().Unix(),
		"model":   model,
		"output":  output,
		"usage": map[string]any{
			"input_tokens":  result.Usage.InputTokens,
			"output_tokens": result.Usage.OutputTokens,
			"total_tokens":  result.Usage.InputTokens + result.Usage.CachedInputTokens + result.Usage.CacheWriteTokens + result.Usage.OutputTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens":      result.Usage.CachedInputTokens,
				"cache_write_tokens": result.Usage.CacheWriteTokens,
			},
		},
		"session_usage": map[string]any{
			"input_tokens":  result.SessionUsage.InputTokens,
			"output_tokens": result.SessionUsage.OutputTokens,
			"total_tokens":  result.SessionUsage.InputTokens + result.SessionUsage.CachedInputTokens + result.SessionUsage.CacheWriteTokens + result.SessionUsage.OutputTokens,
			"input_tokens_details": map[string]any{
				"cached_tokens":      result.SessionUsage.CachedInputTokens,
				"cache_write_tokens": result.SessionUsage.CacheWriteTokens,
			},
		},
	}
}
