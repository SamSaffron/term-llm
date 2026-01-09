package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
)

// OpenAIProvider implements Provider using the standard OpenAI API.
type OpenAIProvider struct {
	client *openai.Client
	model  string
	effort string // reasoning effort: "low", "medium", "high", "xhigh", or ""
}

// parseModelEffort extracts effort suffix from model name.
// "gpt-5.2-high" -> ("gpt-5.2", "high")
// "gpt-5.2-xhigh" -> ("gpt-5.2", "xhigh")
// "gpt-5.2" -> ("gpt-5.2", "")
func parseModelEffort(model string) (string, string) {
	// Check suffixes in order from longest to shortest to avoid "-high" matching "-xhigh"
	suffixes := []string{"xhigh", "medium", "high", "low"}
	for _, effort := range suffixes {
		suffix := "-" + effort
		if strings.HasSuffix(model, suffix) {
			return strings.TrimSuffix(model, suffix), effort
		}
	}
	return model, ""
}

func NewOpenAIProvider(apiKey, model string) *OpenAIProvider {
	actualModel, effort := parseModelEffort(model)
	client := openai.NewClient(option.WithAPIKey(apiKey))
	return &OpenAIProvider{
		client: &client,
		model:  actualModel,
		effort: effort,
	}
}

func (p *OpenAIProvider) Name() string {
	if p.effort != "" {
		return fmt.Sprintf("OpenAI (%s, effort=%s)", p.model, p.effort)
	}
	return fmt.Sprintf("OpenAI (%s)", p.model)
}

func (p *OpenAIProvider) Credential() string {
	return "api_key"
}

func (p *OpenAIProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch: true,
		NativeWebFetch:  false, // No native URL fetch
		ToolCalls:       true,
	}
}

func (p *OpenAIProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		system, inputItems := buildOpenAIInput(req.Messages)
		if len(inputItems) == 0 {
			return fmt.Errorf("no user content provided")
		}

		tools, err := buildOpenAITools(req.Tools)
		if err != nil {
			return err
		}
		if req.Search {
			webSearchTool := responses.ToolParamOfWebSearchPreview(responses.WebSearchToolTypeWebSearchPreview)
			tools = append([]responses.ToolUnionParam{webSearchTool}, tools...)
		}

		// Strip effort suffix from req.Model if present, use it if no provider-level effort set
		reqModel, reqEffort := parseModelEffort(req.Model)
		model := chooseModel(reqModel, p.model)
		effort := p.effort
		if effort == "" && reqEffort != "" {
			effort = reqEffort
		}

		params := responses.ResponseNewParams{
			Model: shared.ResponsesModel(model),
			Input: responses.ResponseNewParamsInputUnion{
				OfInputItemList: inputItems,
			},
			Tools: tools,
		}
		if system != "" {
			params.Instructions = openai.String(system)
		}
		if req.ParallelToolCalls {
			params.ParallelToolCalls = openai.Bool(true)
		}
		if req.MaxOutputTokens > 0 {
			params.MaxOutputTokens = openai.Int(int64(req.MaxOutputTokens))
		}
		if req.Temperature > 0 {
			params.Temperature = openai.Float(float64(req.Temperature))
		}
		if req.TopP > 0 {
			params.TopP = openai.Float(float64(req.TopP))
		}
		if effort != "" {
			params.Reasoning = shared.ReasoningParam{
				Effort: shared.ReasoningEffort(effort),
			}
		}
		if req.ToolChoice.Mode != "" {
			params.ToolChoice = buildOpenAIToolChoice(req.ToolChoice)
		}

		if req.Debug {
			userPreview := collectRoleText(req.Messages, RoleUser)
			fmt.Fprintln(os.Stderr, "=== DEBUG: OpenAI Stream Request ===")
			fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
			fmt.Fprintf(os.Stderr, "System: %s\n", truncate(system, 200))
			fmt.Fprintf(os.Stderr, "User: %s\n", truncate(userPreview, 200))
			fmt.Fprintf(os.Stderr, "Input Items: %d\n", len(inputItems))
			fmt.Fprintf(os.Stderr, "Tools: %d\n", len(tools))
			fmt.Fprintln(os.Stderr, "===================================")
		}

		if len(tools) > 0 {
			resp, err := p.client.Responses.New(ctx, params)
			if err != nil {
				return fmt.Errorf("openai API error: %w", err)
			}
			emitOpenAIResponseOutput(events, resp)
			emitOpenAIUsage(events, resp)
			events <- Event{Type: EventDone}
			return nil
		}

		var lastResp *responses.Response
		stream := p.client.Responses.NewStreaming(ctx, params)
		for stream.Next() {
			event := stream.Current()
			if event.Type == "response.output_text.delta" && event.Text != "" {
				events <- Event{Type: EventTextDelta, Text: event.Text}
			}
			if event.Type == "response.completed" {
				lastResp = &event.Response
			}
		}
		if err := stream.Err(); err != nil {
			return fmt.Errorf("openai streaming error: %w", err)
		}
		if lastResp != nil {
			emitOpenAIUsage(events, lastResp)
		}
		events <- Event{Type: EventDone}
		return nil
	}), nil
}

func buildOpenAITools(specs []ToolSpec) ([]responses.ToolUnionParam, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	tools := make([]responses.ToolUnionParam, 0, len(specs))
	for _, spec := range specs {
		// Normalize schema for OpenAI's strict requirements
		schema := normalizeSchemaForOpenAI(spec.Schema)
		tool := responses.ToolParamOfFunction(spec.Name, schema, true)
		if spec.Description != "" {
			tool.OfFunction.Description = openai.String(spec.Description)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// normalizeSchemaForOpenAI ensures schema meets OpenAI's strict requirements:
// - 'required' must include every key in properties
// - 'additionalProperties' must be false
// - unsupported 'format' values must be removed
func normalizeSchemaForOpenAI(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return schema
	}
	return normalizeSchemaRecursive(deepCopyMap(schema))
}

// deepCopyMap creates a deep copy of a map[string]interface{}
func deepCopyMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case map[string]interface{}:
			result[k] = deepCopyMap(val)
		case []interface{}:
			result[k] = deepCopySlice(val)
		default:
			result[k] = v
		}
	}
	return result
}

func deepCopySlice(s []interface{}) []interface{} {
	if s == nil {
		return nil
	}
	result := make([]interface{}, len(s))
	for i, v := range s {
		switch val := v.(type) {
		case map[string]interface{}:
			result[i] = deepCopyMap(val)
		case []interface{}:
			result[i] = deepCopySlice(val)
		default:
			result[i] = v
		}
	}
	return result
}

// normalizeSchemaRecursive applies OpenAI normalization recursively
func normalizeSchemaRecursive(schema map[string]interface{}) map[string]interface{} {
	// Remove unsupported format values (OpenAI only supports a limited set)
	if format, ok := schema["format"].(string); ok {
		// OpenAI supported formats: date-time, date, time, email
		// Remove uri, uri-reference, hostname, ipv4, ipv6, uuid, etc.
		switch format {
		case "date-time", "date", "time", "email":
			// Keep these
		default:
			delete(schema, "format")
		}
	}

	// Handle properties
	if props, ok := schema["properties"].(map[string]interface{}); ok && len(props) > 0 {
		// Recursively normalize each property
		for key, val := range props {
			if propSchema, ok := val.(map[string]interface{}); ok {
				props[key] = normalizeSchemaRecursive(propSchema)
			}
		}

		// Build required array with all property keys
		required := make([]string, 0, len(props))
		for key := range props {
			required = append(required, key)
		}
		schema["required"] = required
	}

	// Handle array items
	if items, ok := schema["items"].(map[string]interface{}); ok {
		schema["items"] = normalizeSchemaRecursive(items)
	}

	// Handle anyOf, oneOf, allOf
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := schema[key].([]interface{}); ok {
			for i, item := range arr {
				if itemSchema, ok := item.(map[string]interface{}); ok {
					arr[i] = normalizeSchemaRecursive(itemSchema)
				}
			}
		}
	}

	// OpenAI requires additionalProperties to be false for objects
	if schema["type"] == "object" || schema["properties"] != nil {
		schema["additionalProperties"] = false
	}

	return schema
}

func buildOpenAIToolChoice(choice ToolChoice) responses.ResponseNewParamsToolChoiceUnion {
	switch choice.Mode {
	case ToolChoiceNone:
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsNone)}
	case ToolChoiceRequired:
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsRequired)}
	case ToolChoiceName:
		return responses.ResponseNewParamsToolChoiceUnion{OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: choice.Name}}
	default:
		return responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsAuto)}
	}
}

func buildOpenAIInput(messages []Message) (string, responses.ResponseInputParam) {
	var systemParts []string
	inputItems := make(responses.ResponseInputParam, 0, len(messages))

	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			if text := collectTextParts(msg.Parts); text != "" {
				systemParts = append(systemParts, text)
			}
		case RoleUser:
			inputItems = append(inputItems, buildOpenAIMessageItems(responses.EasyInputMessageRoleUser, msg.Parts)...)
		case RoleAssistant:
			inputItems = append(inputItems, buildOpenAIMessageItems(responses.EasyInputMessageRoleAssistant, msg.Parts)...)
		case RoleTool:
			for _, part := range msg.Parts {
				if part.Type != PartToolResult || part.ToolResult == nil {
					continue
				}
				callID := strings.TrimSpace(part.ToolResult.ID)
				if callID == "" {
					continue
				}
				inputItems = append(inputItems, responses.ResponseInputItemParamOfFunctionCallOutput(callID, part.ToolResult.Content))
			}
		}
	}

	return strings.Join(systemParts, "\n\n"), inputItems
}

func buildOpenAIMessageItems(role responses.EasyInputMessageRole, parts []Part) []responses.ResponseInputItemUnionParam {
	var items []responses.ResponseInputItemUnionParam
	var textBuf strings.Builder

	flushText := func() {
		if textBuf.Len() == 0 {
			return
		}
		items = append(items, responses.ResponseInputItemParamOfMessage(textBuf.String(), role))
		textBuf.Reset()
	}

	for _, part := range parts {
		switch part.Type {
		case PartText:
			if part.Text != "" {
				textBuf.WriteString(part.Text)
			}
		case PartToolCall:
			if part.ToolCall == nil {
				continue
			}
			flushText()
			callID := strings.TrimSpace(part.ToolCall.ID)
			if callID == "" {
				continue
			}
			args := strings.TrimSpace(string(part.ToolCall.Arguments))
			if args == "" {
				args = "{}"
			}
			items = append(items, responses.ResponseInputItemParamOfFunctionCall(args, callID, part.ToolCall.Name))
		}
	}

	flushText()
	return items
}

func emitOpenAIUsage(events chan<- Event, resp *responses.Response) {
	if resp.Usage.OutputTokens > 0 {
		events <- Event{Type: EventUsage, Use: &Usage{
			InputTokens:  int(resp.Usage.InputTokens),
			OutputTokens: int(resp.Usage.OutputTokens),
		}}
	}
}

func emitOpenAIResponseOutput(events chan<- Event, resp *responses.Response) {
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				switch content.Type {
				case "output_text":
					if content.Text != "" {
						events <- Event{Type: EventTextDelta, Text: content.Text}
					}
				case "refusal":
					if content.Refusal != "" {
						events <- Event{Type: EventTextDelta, Text: content.Refusal}
					}
				}
			}
		case "function_call":
			callID := strings.TrimSpace(item.ID)
			if callID == "" {
				callID = strings.TrimSpace(item.CallID)
			}
			args := strings.TrimSpace(item.Arguments)
			var raw json.RawMessage
			if args != "" {
				raw = json.RawMessage(args)
			}
			call := ToolCall{
				ID:        callID,
				Name:      item.Name,
				Arguments: raw,
			}
			events <- Event{Type: EventToolCall, Tool: &call}
		}
	}
}

func collectRoleText(messages []Message, role Role) string {
	var parts []string
	for _, msg := range messages {
		if msg.Role != role {
			continue
		}
		if text := collectTextParts(msg.Parts); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}
