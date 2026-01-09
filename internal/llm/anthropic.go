package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// ListModels returns available models from Anthropic.
func (p *AnthropicProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	page, err := p.client.Models.List(ctx, anthropic.ModelListParams{})
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}

	var models []ModelInfo
	for _, m := range page.Data {
		models = append(models, ModelInfo{
			ID:          m.ID,
			DisplayName: m.DisplayName,
			Created:     m.CreatedAt.Unix(),
		})
	}

	return models, nil
}

// AnthropicProvider implements Provider using the Anthropic API.
type AnthropicProvider struct {
	client         *anthropic.Client
	model          string
	thinkingBudget int64  // 0 = disabled, >0 = enabled with budget
	credential     string // "claude-code" or "api_key"
}

// parseModelThinking extracts -thinking suffix from model name.
// "claude-sonnet-4-5-thinking" -> ("claude-sonnet-4-5", 10000)
// "claude-sonnet-4-5" -> ("claude-sonnet-4-5", 0)
func parseModelThinking(model string) (string, int64) {
	if strings.HasSuffix(model, "-thinking") {
		return strings.TrimSuffix(model, "-thinking"), 10000
	}
	return model, 0
}

func NewAnthropicProvider(apiKey, model string) *AnthropicProvider {
	actualModel, thinkingBudget := parseModelThinking(model)
	var client anthropic.Client
	var credential string
	if apiKey != "" {
		// OAuth tokens (from Claude Code) start with "sk-ant-oat" and need Bearer auth
		// Standard API keys start with "sk-ant-api" and use x-api-key header
		if strings.HasPrefix(apiKey, "sk-ant-oat") {
			client = anthropic.NewClient(option.WithAuthToken(apiKey))
			credential = "claude"
		} else {
			client = anthropic.NewClient(option.WithAPIKey(apiKey))
			credential = "api_key"
		}
	} else {
		client = anthropic.NewClient()
		credential = "env"
	}
	return &AnthropicProvider{
		client:         &client,
		model:          actualModel,
		thinkingBudget: thinkingBudget,
		credential:     credential,
	}
}

func (p *AnthropicProvider) Name() string {
	if p.thinkingBudget > 0 {
		return fmt.Sprintf("Anthropic (%s, thinking=%dk)", p.model, p.thinkingBudget/1000)
	}
	return fmt.Sprintf("Anthropic (%s)", p.model)
}

func (p *AnthropicProvider) Credential() string {
	return p.credential
}

func (p *AnthropicProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch: true,
		NativeWebFetch:  true,
		ToolCalls:       true,
	}
}

func (p *AnthropicProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	if req.Search {
		return p.streamWithSearch(ctx, req)
	}
	return p.streamStandard(ctx, req)
}

func (p *AnthropicProvider) streamStandard(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		system, messages := buildAnthropicMessages(req.Messages)
		accumulator := newToolCallAccumulator()

		params := anthropic.MessageNewParams{
			Model:     anthropic.Model(chooseModel(req.Model, p.model)),
			MaxTokens: maxTokens(req.MaxOutputTokens, 4096),
			Messages:  messages,
		}
		if system != "" {
			params.System = []anthropic.TextBlockParam{{Text: system}}
		}
		if len(req.Tools) > 0 {
			params.Tools = buildAnthropicTools(req.Tools)
			if p.thinkingBudget == 0 {
				params.ToolChoice = buildAnthropicToolChoice(req.ToolChoice, req.ParallelToolCalls)
			}
		}

		if p.thinkingBudget > 0 {
			params.MaxTokens = maxTokens(req.MaxOutputTokens, 16000)
			params.Thinking = anthropic.ThinkingConfigParamUnion{
				OfEnabled: &anthropic.ThinkingConfigEnabledParam{
					BudgetTokens: p.thinkingBudget,
				},
			}
		}

		if req.Debug {
			fmt.Fprintln(os.Stderr, "=== DEBUG: Anthropic Stream Request ===")
			fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
			fmt.Fprintf(os.Stderr, "System: %s\n", truncate(system, 200))
			fmt.Fprintf(os.Stderr, "Messages: %d\n", len(messages))
			fmt.Fprintf(os.Stderr, "Tools: %d\n", len(req.Tools))
			fmt.Fprintln(os.Stderr, "======================================")
		}

		var lastUsage *Usage
		stream := p.client.Messages.NewStreaming(ctx, params)
		for stream.Next() {
			event := stream.Current()
			switch variant := event.AsAny().(type) {
			case anthropic.ContentBlockDeltaEvent:
				switch delta := variant.Delta.AsAny().(type) {
				case anthropic.InputJSONDelta:
					if delta.PartialJSON != "" {
						accumulator.Append(variant.Index, delta.PartialJSON)
					}
				case anthropic.TextDelta:
					if delta.Text != "" {
						events <- Event{Type: EventTextDelta, Text: delta.Text}
					}
				}
			case anthropic.ContentBlockStartEvent:
				if toolCall, ok := anthropicToolCall(variant.ContentBlock); ok {
					accumulator.Start(variant.Index, toolCall)
				}
			case anthropic.ContentBlockStopEvent:
				if toolCall, ok := accumulator.Finish(variant.Index); ok {
					events <- Event{Type: EventToolCall, Tool: &toolCall}
				}
			case anthropic.MessageDeltaEvent:
				if variant.Usage.OutputTokens > 0 {
					lastUsage = &Usage{
						InputTokens:  int(variant.Usage.InputTokens),
						OutputTokens: int(variant.Usage.OutputTokens),
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			return fmt.Errorf("anthropic streaming error: %w", err)
		}
		if lastUsage != nil {
			events <- Event{Type: EventUsage, Use: lastUsage}
		}
		events <- Event{Type: EventDone}
		return nil
	}), nil
}

func (p *AnthropicProvider) streamWithSearch(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		system, messages := buildAnthropicBetaMessages(req.Messages)
		accumulator := newToolCallAccumulator()

		tools := buildAnthropicBetaTools(req.Tools)
		webSearchTool := anthropic.BetaToolUnionParam{
			OfWebSearchTool20250305: &anthropic.BetaWebSearchTool20250305Param{
				MaxUses: anthropic.Int(5),
			},
		}
		webFetchTool := anthropic.BetaToolUnionParam{
			OfWebFetchTool20250910: &anthropic.BetaWebFetchTool20250910Param{
				MaxUses: anthropic.Int(3),
			},
		}
		tools = append([]anthropic.BetaToolUnionParam{webSearchTool, webFetchTool}, tools...)

		params := anthropic.BetaMessageNewParams{
			Model:     anthropic.Model(chooseModel(req.Model, p.model)),
			MaxTokens: maxTokens(req.MaxOutputTokens, 4096),
			Betas:     []anthropic.AnthropicBeta{"web-search-2025-03-05", "web-fetch-2025-09-10"},
			Messages:  messages,
			Tools:     tools,
		}
		if system != "" {
			params.System = []anthropic.BetaTextBlockParam{{Text: system}}
		}
		if len(req.Tools) > 0 && p.thinkingBudget == 0 {
			params.ToolChoice = buildAnthropicBetaToolChoice(req.ToolChoice, req.ParallelToolCalls)
		}

		if p.thinkingBudget > 0 {
			params.MaxTokens = maxTokens(req.MaxOutputTokens, 16000)
			params.Thinking = anthropic.BetaThinkingConfigParamUnion{
				OfEnabled: &anthropic.BetaThinkingConfigEnabledParam{
					BudgetTokens: p.thinkingBudget,
				},
			}
		}

		if req.Debug {
			fmt.Fprintln(os.Stderr, "=== DEBUG: Anthropic Stream Request (search) ===")
			fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
			fmt.Fprintf(os.Stderr, "System: %s\n", truncate(system, 200))
			fmt.Fprintf(os.Stderr, "Messages: %d\n", len(messages))
			fmt.Fprintf(os.Stderr, "Tools: %d (includes web_search, web_fetch)\n", len(tools))
			fmt.Fprintln(os.Stderr, "================================================")
		}

		// Track if we're in a server tool use block (web_search, etc.)
		inServerTool := false
		var lastUsage *Usage

		stream := p.client.Beta.Messages.NewStreaming(ctx, params)
		for stream.Next() {
			event := stream.Current()
			switch variant := event.AsAny().(type) {
			case anthropic.BetaRawContentBlockDeltaEvent:
				switch delta := variant.Delta.AsAny().(type) {
				case anthropic.BetaInputJSONDelta:
					if delta.PartialJSON != "" {
						accumulator.Append(variant.Index, delta.PartialJSON)
					}
				case anthropic.BetaTextDelta:
					if delta.Text != "" {
						// If we were in a server tool, signal back to thinking/responding
						if inServerTool {
							inServerTool = false
							events <- Event{Type: EventToolExecStart, ToolName: ""}
						}
						events <- Event{Type: EventTextDelta, Text: delta.Text}
					}
				}
			case anthropic.BetaRawContentBlockStartEvent:
				blockType := variant.ContentBlock.Type
				if blockType == "server_tool_use" {
					// Server tool (web_search, etc.) is starting
					serverTool := variant.ContentBlock.AsServerToolUse()
					toolName := string(serverTool.Name)
					inServerTool = true
					events <- Event{Type: EventToolExecStart, ToolName: toolName}
				} else if toolCall, ok := anthropicBetaToolCall(variant.ContentBlock); ok {
					accumulator.Start(variant.Index, toolCall)
				}
			case anthropic.BetaRawContentBlockStopEvent:
				if toolCall, ok := accumulator.Finish(variant.Index); ok {
					events <- Event{Type: EventToolCall, Tool: &toolCall}
				}
			case anthropic.BetaRawMessageDeltaEvent:
				if variant.Usage.OutputTokens > 0 {
					lastUsage = &Usage{
						InputTokens:  int(variant.Usage.InputTokens),
						OutputTokens: int(variant.Usage.OutputTokens),
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			return fmt.Errorf("anthropic streaming error: %w", err)
		}
		if lastUsage != nil {
			events <- Event{Type: EventUsage, Use: lastUsage}
		}
		events <- Event{Type: EventDone}
		return nil
	}), nil
}

func buildAnthropicMessages(messages []Message) (string, []anthropic.MessageParam) {
	var systemParts []string
	var out []anthropic.MessageParam

	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			systemParts = append(systemParts, collectTextParts(msg.Parts))
		case RoleUser:
			blocks := buildAnthropicBlocks(msg.Parts, false)
			if len(blocks) > 0 {
				out = append(out, anthropic.NewUserMessage(blocks...))
			}
		case RoleAssistant:
			blocks := buildAnthropicBlocks(msg.Parts, true)
			if len(blocks) > 0 {
				out = append(out, anthropic.NewAssistantMessage(blocks...))
			}
		case RoleTool:
			blocks := buildAnthropicBlocks(msg.Parts, false)
			if len(blocks) > 0 {
				out = append(out, anthropic.NewUserMessage(blocks...))
			}
		}
	}

	return strings.Join(systemParts, "\n\n"), out
}

func buildAnthropicBetaMessages(messages []Message) (string, []anthropic.BetaMessageParam) {
	var systemParts []string
	var out []anthropic.BetaMessageParam

	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			systemParts = append(systemParts, collectTextParts(msg.Parts))
		case RoleUser:
			blocks := buildAnthropicBetaBlocks(msg.Parts, false)
			if len(blocks) > 0 {
				out = append(out, anthropic.NewBetaUserMessage(blocks...))
			}
		case RoleAssistant:
			blocks := buildAnthropicBetaBlocks(msg.Parts, true)
			if len(blocks) > 0 {
				out = append(out, anthropic.BetaMessageParam{
					Role:    anthropic.BetaMessageParamRoleAssistant,
					Content: blocks,
				})
			}
		case RoleTool:
			blocks := buildAnthropicBetaBlocks(msg.Parts, false)
			if len(blocks) > 0 {
				out = append(out, anthropic.NewBetaUserMessage(blocks...))
			}
		}
	}

	return strings.Join(systemParts, "\n\n"), out
}

func buildAnthropicBlocks(parts []Part, allowToolUse bool) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case PartText:
			if part.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(part.Text))
			}
		case PartToolCall:
			if allowToolUse && part.ToolCall != nil {
				blocks = append(blocks, anthropic.NewToolUseBlock(part.ToolCall.ID, part.ToolCall.Arguments, part.ToolCall.Name))
			}
		case PartToolResult:
			if part.ToolResult != nil {
				blocks = append(blocks, anthropic.NewToolResultBlock(part.ToolResult.ID, part.ToolResult.Content, false))
			}
		}
	}
	return blocks
}

func buildAnthropicBetaBlocks(parts []Part, allowToolUse bool) []anthropic.BetaContentBlockParamUnion {
	blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case PartText:
			if part.Text != "" {
				blocks = append(blocks, anthropic.NewBetaTextBlock(part.Text))
			}
		case PartToolCall:
			if allowToolUse && part.ToolCall != nil {
				blocks = append(blocks, anthropic.NewBetaToolUseBlock(part.ToolCall.ID, part.ToolCall.Arguments, part.ToolCall.Name))
			}
		case PartToolResult:
			if part.ToolResult != nil {
				blocks = append(blocks, betaToolResultBlock(part.ToolResult.ID, part.ToolResult.Content))
			}
		}
	}
	return blocks
}

func betaToolResultBlock(id, content string) anthropic.BetaContentBlockParamUnion {
	block := anthropic.BetaToolResultBlockParam{
		ToolUseID: id,
		Content: []anthropic.BetaToolResultBlockParamContentUnion{{
			OfText: &anthropic.BetaTextBlockParam{Text: content},
		}},
	}
	return anthropic.BetaContentBlockParamUnion{OfToolResult: &block}
}

func buildAnthropicTools(specs []ToolSpec) []anthropic.ToolUnionParam {
	if len(specs) == 0 {
		return nil
	}
	tools := make([]anthropic.ToolUnionParam, 0, len(specs))
	for _, spec := range specs {
		inputSchema := anthropic.ToolInputSchemaParam{
			Type:       constant.Object("object"),
			Properties: spec.Schema["properties"],
			Required:   schemaRequired(spec.Schema),
		}
		tool := anthropic.ToolUnionParamOfTool(inputSchema, spec.Name)
		if spec.Description != "" {
			tool.OfTool.Description = anthropic.String(spec.Description)
		}
		tools = append(tools, tool)
	}
	return tools
}

func buildAnthropicBetaTools(specs []ToolSpec) []anthropic.BetaToolUnionParam {
	if len(specs) == 0 {
		return nil
	}
	tools := make([]anthropic.BetaToolUnionParam, 0, len(specs))
	for _, spec := range specs {
		inputSchema := anthropic.BetaToolInputSchemaParam{
			Type:       constant.Object("object"),
			Properties: spec.Schema["properties"],
			Required:   schemaRequired(spec.Schema),
		}
		tool := anthropic.BetaToolUnionParam{
			OfTool: &anthropic.BetaToolParam{
				Name:        spec.Name,
				Description: anthropic.String(spec.Description),
				InputSchema: inputSchema,
			},
		}
		tools = append(tools, tool)
	}
	return tools
}

func buildAnthropicToolChoice(choice ToolChoice, parallel bool) anthropic.ToolChoiceUnionParam {
	disableParallel := !parallel
	switch choice.Mode {
	case ToolChoiceNone:
		none := anthropic.NewToolChoiceNoneParam()
		return anthropic.ToolChoiceUnionParam{OfNone: &none}
	case ToolChoiceRequired:
		return anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
	case ToolChoiceName:
		return anthropic.ToolChoiceParamOfTool(choice.Name)
	default:
		return anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{DisableParallelToolUse: anthropic.Bool(disableParallel)}}
	}
}

func buildAnthropicBetaToolChoice(choice ToolChoice, parallel bool) anthropic.BetaToolChoiceUnionParam {
	disableParallel := !parallel
	switch choice.Mode {
	case ToolChoiceNone:
		none := anthropic.NewBetaToolChoiceNoneParam()
		return anthropic.BetaToolChoiceUnionParam{OfNone: &none}
	case ToolChoiceRequired:
		return anthropic.BetaToolChoiceUnionParam{OfAny: &anthropic.BetaToolChoiceAnyParam{}}
	case ToolChoiceName:
		return anthropic.BetaToolChoiceParamOfTool(choice.Name)
	default:
		return anthropic.BetaToolChoiceUnionParam{OfAuto: &anthropic.BetaToolChoiceAutoParam{DisableParallelToolUse: anthropic.Bool(disableParallel)}}
	}
}

func anthropicToolCall(block anthropic.ContentBlockStartEventContentBlockUnion) (ToolCall, bool) {
	if variant, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
		return ToolCall{ID: variant.ID, Name: variant.Name, Arguments: toolInputToRaw(variant.Input)}, true
	}
	return ToolCall{}, false
}

func anthropicBetaToolCall(block anthropic.BetaRawContentBlockStartEventContentBlockUnion) (ToolCall, bool) {
	if variant, ok := block.AsAny().(anthropic.BetaToolUseBlock); ok {
		return ToolCall{ID: variant.ID, Name: variant.Name, Arguments: toolInputToRaw(variant.Input)}, true
	}
	return ToolCall{}, false
}

func toolInputToRaw(input any) json.RawMessage {
	switch v := input.(type) {
	case json.RawMessage:
		return v
	case []byte:
		return json.RawMessage(v)
	case string:
		return json.RawMessage(v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return json.RawMessage(data)
	}
}

type toolCallAccumulator struct {
	calls    map[int64]ToolCall
	fallback map[int64]json.RawMessage
	partial  map[int64]*strings.Builder
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{
		calls:    make(map[int64]ToolCall),
		fallback: make(map[int64]json.RawMessage),
		partial:  make(map[int64]*strings.Builder),
	}
}

func (a *toolCallAccumulator) Start(index int64, call ToolCall) {
	if len(call.Arguments) > 0 {
		a.fallback[index] = call.Arguments
	}
	call.Arguments = nil
	a.calls[index] = call
}

func (a *toolCallAccumulator) Append(index int64, partial string) {
	if partial == "" {
		return
	}
	builder := a.partial[index]
	if builder == nil {
		builder = &strings.Builder{}
		a.partial[index] = builder
	}
	builder.WriteString(partial)
}

func (a *toolCallAccumulator) Finish(index int64) (ToolCall, bool) {
	call, ok := a.calls[index]
	if !ok {
		return ToolCall{}, false
	}
	if builder := a.partial[index]; builder != nil && builder.Len() > 0 {
		call.Arguments = json.RawMessage(builder.String())
	} else if fallback, ok := a.fallback[index]; ok {
		call.Arguments = fallback
	}
	delete(a.calls, index)
	delete(a.partial, index)
	delete(a.fallback, index)
	return call, true
}

func maxTokens(requested, fallback int) int64 {
	if requested > 0 {
		return int64(requested)
	}
	return int64(fallback)
}
