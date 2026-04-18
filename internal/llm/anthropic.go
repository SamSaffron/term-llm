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
			InputLimit:  InputLimitForModel(m.ID),
		})
	}

	return models, nil
}

// Anthropic credential mode constants for the config "credentials" field.
// These control which authentication method is used. "auto" (or empty) uses
// the default cascade; any other value forces that specific method.
const (
	AnthropicCredAuto   = "auto"    // Default cascade: api_key → env
	AnthropicCredAPIKey = "api_key" // Force: explicit api_key from config only
	AnthropicCredEnv    = "env"     // Force: ANTHROPIC_API_KEY env var only
)

// AnthropicProvider implements Provider using the Anthropic API.
type AnthropicProvider struct {
	client         *anthropic.Client
	model          string
	thinkingBudget int64  // 0 = disabled, >0 = enabled with budget
	useAdaptive    bool   // true = adaptive thinking (-thinking on 4.6 models)
	use1m          bool   // true = 1M token context window (-1m suffix)
	credential     string // "api_key" or "env"
}

// isAdaptiveModel returns true for Claude 4.6+ models that support adaptive thinking.
func isAdaptiveModel(model string) bool {
	return strings.HasPrefix(model, "claude-sonnet-4-6") ||
		strings.HasPrefix(model, "claude-opus-4-6") ||
		strings.HasPrefix(model, "claude-opus-4-7")
}

// parseModelThinking extracts -thinking suffix from model name.
// For 4.6 models, -thinking uses adaptive thinking (budget_tokens is deprecated).
// For older models, -thinking uses budget_tokens as before.
//
// "claude-sonnet-4-6-thinking" -> ("claude-sonnet-4-6", 0, true)
// "claude-haiku-4-5-thinking"  -> ("claude-haiku-4-5", 10000, false)
// "claude-sonnet-4-6"          -> ("claude-sonnet-4-6", 0, false)
func parseModelThinking(model string) (string, int64, bool) {
	if strings.HasSuffix(model, "-thinking") {
		base := strings.TrimSuffix(model, "-thinking")
		if isAdaptiveModel(base) {
			return base, 0, true
		}
		return base, 10000, false
	}
	return model, 0, false
}

// the1mBetaHeader is the beta header that enables the 1M token context window.
// Available for claude-sonnet-4-6, claude-sonnet-4-5, claude-sonnet-4, claude-opus-4-6, claude-opus-4-7.
// Requires Anthropic usage tier 4 or custom rate limits.
const the1mBetaHeader = "context-1m-2025-08-07"

// parseModel1m extracts the -1m suffix from a model name.
// Returns the base model name and whether 1M context is requested.
//
// "claude-sonnet-4-6-1m"         -> ("claude-sonnet-4-6", true)
// "claude-sonnet-4-6-1m-thinking" is handled upstream (thinking stripped first)
// "claude-sonnet-4-6"            -> ("claude-sonnet-4-6", false)
func parseModel1m(model string) (string, bool) {
	if strings.HasSuffix(model, "-1m") {
		return strings.TrimSuffix(model, "-1m"), true
	}
	return model, false
}

// NewAnthropicProvider creates a new Anthropic provider.
// The credentialMode parameter controls which authentication method is used:
//   - "" or "auto": try the cascade (api_key → env)
//   - "api_key":    use only the explicit apiKey parameter
//   - "env":        use only the ANTHROPIC_API_KEY environment variable
func NewAnthropicProvider(apiKey, model, credentialMode string) (*AnthropicProvider, error) {
	// Strip -thinking first (may leave -1m), then strip -1m.
	// This means claude-sonnet-4-6-1m-thinking works correctly:
	//   step 1: strip -thinking -> "claude-sonnet-4-6-1m", adaptive=true
	//   step 2: strip -1m       -> "claude-sonnet-4-6",    use1m=true
	afterThinking, thinkingBudget, adaptive := parseModelThinking(model)
	actualModel, use1m := parseModel1m(afterThinking)

	// Normalize empty credential mode to "auto"
	if credentialMode == "" {
		credentialMode = AnthropicCredAuto
	}

	mkProvider := func(client anthropic.Client, cred string) *AnthropicProvider {
		return &AnthropicProvider{
			client:         &client,
			model:          actualModel,
			thinkingBudget: thinkingBudget,
			useAdaptive:    adaptive,
			use1m:          use1m,
			credential:     cred,
		}
	}

	// When a specific mode is forced, only try that one source.
	switch credentialMode {
	case AnthropicCredAPIKey:
		if apiKey == "" {
			return nil, fmt.Errorf("credentials mode %q requires an explicit api_key in provider config", credentialMode)
		}
		return mkProvider(anthropic.NewClient(option.WithAPIKey(apiKey)), "api_key"), nil

	case AnthropicCredEnv:
		envKey := os.Getenv("ANTHROPIC_API_KEY")
		if envKey == "" {
			return nil, fmt.Errorf("credentials mode %q requires ANTHROPIC_API_KEY environment variable", credentialMode)
		}
		return mkProvider(anthropic.NewClient(option.WithAPIKey(envKey)), "env"), nil

	case AnthropicCredAuto:
		// Fall through to the cascade below.

	default:
		return nil, fmt.Errorf("unknown Anthropic credentials mode: %q (valid: auto, api_key, env)", credentialMode)
	}

	// Auto mode: full credential cascade.

	// 1. Explicit API key provided (from config)
	if apiKey != "" {
		return mkProvider(anthropic.NewClient(option.WithAPIKey(apiKey)), "api_key"), nil
	}

	// 2. ANTHROPIC_API_KEY environment variable
	if envKey := os.Getenv("ANTHROPIC_API_KEY"); envKey != "" {
		return mkProvider(anthropic.NewClient(option.WithAPIKey(envKey)), "env"), nil
	}

	return nil, fmt.Errorf("no Anthropic credentials found. Set ANTHROPIC_API_KEY or configure api_key in provider config")
}

func (p *AnthropicProvider) Name() string {
	suffix := ""
	if p.use1m {
		suffix = ", 1m"
	}
	if p.useAdaptive {
		return fmt.Sprintf("Anthropic (%s, adaptive%s)", p.model, suffix)
	}
	if p.thinkingBudget > 0 {
		return fmt.Sprintf("Anthropic (%s, thinking=%dk%s)", p.model, p.thinkingBudget/1000, suffix)
	}
	if p.use1m {
		return fmt.Sprintf("Anthropic (%s, 1m)", p.model)
	}
	return fmt.Sprintf("Anthropic (%s)", p.model)
}

func (p *AnthropicProvider) Credential() string {
	return p.credential
}

func (p *AnthropicProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    true,
		NativeWebFetch:     true,
		ToolCalls:          true,
		SupportsToolChoice: true,
	}
}

func (p *AnthropicProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	req.MaxOutputTokens = ClampOutputTokens(req.MaxOutputTokens, chooseModel(req.Model, p.model))
	if req.Search {
		return p.streamWithSearch(ctx, req)
	}
	return p.streamStandard(ctx, req)
}

func (p *AnthropicProvider) streamStandard(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		system, messages := buildAnthropicMessages(req.Messages)
		applyLastMessageCacheControl(messages)
		accumulator := newToolCallAccumulator()

		params := anthropic.MessageNewParams{
			Model:     anthropic.Model(chooseModel(req.Model, p.model)),
			MaxTokens: maxTokens(req.MaxOutputTokens, 4096),
			Messages:  messages,
		}
		if system != "" {
			params.System = []anthropic.TextBlockParam{{
				Text:         system,
				CacheControl: anthropic.NewCacheControlEphemeralParam(),
			}}
		}
		if len(req.Tools) > 0 {
			params.Tools = buildAnthropicTools(req.Tools)
			if p.thinkingBudget == 0 && !p.useAdaptive {
				params.ToolChoice = buildAnthropicToolChoice(req.ToolChoice, req.ParallelToolCalls)
			}
		}

		if p.useAdaptive {
			params.MaxTokens = maxTokens(req.MaxOutputTokens, 16000)
			params.Thinking = anthropic.ThinkingConfigParamUnion{
				OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
			}
		} else if p.thinkingBudget > 0 {
			params.MaxTokens = maxTokens(req.MaxOutputTokens, 16000)
			params.Thinking = anthropic.ThinkingConfigParamUnion{
				OfEnabled: &anthropic.ThinkingConfigEnabledParam{
					BudgetTokens: p.thinkingBudget,
				},
			}
			params.OutputConfig = anthropic.OutputConfigParam{
				Effort: anthropic.OutputConfigEffortMax,
			}
		}
		if eff := strings.TrimSpace(req.ReasoningEffort); eff != "" {
			params.OutputConfig = anthropic.OutputConfigParam{
				Effort: anthropic.OutputConfigEffort(strings.ToLower(eff)),
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
		var streamOpts []option.RequestOption
		if p.use1m {
			streamOpts = append(streamOpts, option.WithHeaderAdd("anthropic-beta", the1mBetaHeader))
		}
		stream := p.client.Messages.NewStreaming(ctx, params, streamOpts...)
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
						if err := send.Send(Event{Type: EventTextDelta, Text: delta.Text}); err != nil {
							return err
						}
					}
				case anthropic.ThinkingDelta:
					if err := emitReasoningDelta(send, delta.Thinking, ""); err != nil {
						return err
					}
				case anthropic.SignatureDelta:
					if err := emitReasoningDelta(send, "", delta.Signature); err != nil {
						return err
					}
				}
			case anthropic.ContentBlockStartEvent:
				if err := handleAnthropicStartBlockContent(send, variant.ContentBlock.AsAny(), variant.Index, accumulator); err != nil {
					return err
				}
			case anthropic.ContentBlockStopEvent:
				if toolCall, ok := accumulator.Finish(variant.Index); ok {
					if err := send.Send(Event{Type: EventToolCall, Tool: &toolCall}); err != nil {
						return err
					}
				}
			case anthropic.MessageStartEvent:
				lastUsage = &Usage{
					InputTokens:       int(variant.Message.Usage.InputTokens),
					CachedInputTokens: int(variant.Message.Usage.CacheReadInputTokens),
					CacheWriteTokens:  int(variant.Message.Usage.CacheCreationInputTokens),
				}
			case anthropic.MessageDeltaEvent:
				if variant.Usage.OutputTokens > 0 {
					if lastUsage == nil {
						lastUsage = &Usage{}
					}
					lastUsage.OutputTokens = int(variant.Usage.OutputTokens)
					if lastUsage.InputTokens == 0 && variant.Usage.InputTokens > 0 {
						lastUsage.InputTokens = int(variant.Usage.InputTokens)
					}
					if lastUsage.CachedInputTokens == 0 {
						lastUsage.CachedInputTokens = int(variant.Usage.CacheReadInputTokens)
					}
					if lastUsage.CacheWriteTokens == 0 {
						lastUsage.CacheWriteTokens = int(variant.Usage.CacheCreationInputTokens)
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			return fmt.Errorf("anthropic streaming error: %w", err)
		}
		if lastUsage != nil {
			if err := send.Send(Event{Type: EventUsage, Use: lastUsage}); err != nil {
				return err
			}
		}
		if err := send.Send(Event{Type: EventDone}); err != nil {
			return err
		}
		return nil
	}), nil
}

func (p *AnthropicProvider) streamWithSearch(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		system, messages := buildAnthropicBetaMessages(req.Messages)
		applyBetaLastMessageCacheControl(messages)
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

		betas := []anthropic.AnthropicBeta{"web-search-2025-03-05", "web-fetch-2025-09-10"}
		if p.use1m {
			betas = append(betas, the1mBetaHeader)
		}
		params := anthropic.BetaMessageNewParams{
			Model:     anthropic.Model(chooseModel(req.Model, p.model)),
			MaxTokens: maxTokens(req.MaxOutputTokens, 4096),
			Betas:     betas,
			Messages:  messages,
			Tools:     tools,
		}
		if system != "" {
			params.System = []anthropic.BetaTextBlockParam{{
				Text:         system,
				CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
			}}
		}
		// In search mode, use auto tool choice so model can call web_search first
		// The model will call the user's requested tool after searching
		if len(req.Tools) > 0 && p.thinkingBudget == 0 && !p.useAdaptive {
			params.ToolChoice = anthropic.BetaToolChoiceUnionParam{
				OfAuto: &anthropic.BetaToolChoiceAutoParam{
					DisableParallelToolUse: anthropic.Bool(!req.ParallelToolCalls),
				},
			}
		}

		if p.useAdaptive {
			params.MaxTokens = maxTokens(req.MaxOutputTokens, 16000)
			params.Thinking = anthropic.BetaThinkingConfigParamUnion{
				OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{},
			}
		} else if p.thinkingBudget > 0 {
			params.MaxTokens = maxTokens(req.MaxOutputTokens, 16000)
			params.Thinking = anthropic.BetaThinkingConfigParamUnion{
				OfEnabled: &anthropic.BetaThinkingConfigEnabledParam{
					BudgetTokens: p.thinkingBudget,
				},
			}
			params.OutputConfig = anthropic.BetaOutputConfigParam{
				Effort: anthropic.BetaOutputConfigEffortMax,
			}
		}
		if eff := strings.TrimSpace(req.ReasoningEffort); eff != "" {
			params.OutputConfig = anthropic.BetaOutputConfigParam{
				Effort: anthropic.BetaOutputConfigEffort(strings.ToLower(eff)),
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

		// Track current server tool use block (web_search, etc.)
		currentServerTool := ""
		currentServerToolIndex := int64(-1)
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
						// If we were in a server tool, emit tool end event
						if currentServerTool != "" {
							if err := send.Send(Event{Type: EventToolExecEnd, ToolName: currentServerTool, ToolSuccess: true}); err != nil {
								return err
							}
							currentServerTool = ""
							currentServerToolIndex = -1
						}
						if err := send.Send(Event{Type: EventTextDelta, Text: delta.Text}); err != nil {
							return err
						}
					}
				case anthropic.BetaThinkingDelta:
					if err := emitReasoningDelta(send, delta.Thinking, ""); err != nil {
						return err
					}
				case anthropic.BetaSignatureDelta:
					if err := emitReasoningDelta(send, "", delta.Signature); err != nil {
						return err
					}
				}
			case anthropic.BetaRawContentBlockStartEvent:
				blockType := variant.ContentBlock.Type
				if blockType == "server_tool_use" {
					// Server tool (web_search, etc.) is starting
					serverTool := variant.ContentBlock.AsServerToolUse()
					toolName := string(serverTool.Name)
					currentServerTool = toolName
					currentServerToolIndex = variant.Index
					if err := send.Send(Event{Type: EventToolExecStart, ToolName: toolName}); err != nil {
						return err
					}
				} else {
					if err := handleAnthropicBetaStartBlockContent(send, variant.ContentBlock.AsAny(), variant.Index, accumulator); err != nil {
						return err
					}
				}
			case anthropic.BetaRawContentBlockStopEvent:
				if currentServerTool != "" && variant.Index == currentServerToolIndex {
					if err := send.Send(Event{Type: EventToolExecEnd, ToolName: currentServerTool, ToolSuccess: true}); err != nil {
						return err
					}
					currentServerTool = ""
					currentServerToolIndex = -1
				}
				if toolCall, ok := accumulator.Finish(variant.Index); ok {
					if err := send.Send(Event{Type: EventToolCall, Tool: &toolCall}); err != nil {
						return err
					}
				}
			case anthropic.BetaRawMessageStartEvent:
				lastUsage = &Usage{
					InputTokens:       int(variant.Message.Usage.InputTokens),
					CachedInputTokens: int(variant.Message.Usage.CacheReadInputTokens),
					CacheWriteTokens:  int(variant.Message.Usage.CacheCreationInputTokens),
				}
			case anthropic.BetaRawMessageDeltaEvent:
				if variant.Usage.OutputTokens > 0 {
					if lastUsage == nil {
						lastUsage = &Usage{}
					}
					lastUsage.OutputTokens = int(variant.Usage.OutputTokens)
					if lastUsage.InputTokens == 0 && variant.Usage.InputTokens > 0 {
						lastUsage.InputTokens = int(variant.Usage.InputTokens)
					}
					if lastUsage.CachedInputTokens == 0 {
						lastUsage.CachedInputTokens = int(variant.Usage.CacheReadInputTokens)
					}
					if lastUsage.CacheWriteTokens == 0 {
						lastUsage.CacheWriteTokens = int(variant.Usage.CacheCreationInputTokens)
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			return fmt.Errorf("anthropic streaming error: %w", err)
		}
		if currentServerTool != "" {
			if err := send.Send(Event{Type: EventToolExecEnd, ToolName: currentServerTool, ToolSuccess: true}); err != nil {
				return err
			}
		}
		if lastUsage != nil {
			if err := send.Send(Event{Type: EventUsage, Use: lastUsage}); err != nil {
				return err
			}
		}
		if err := send.Send(Event{Type: EventDone}); err != nil {
			return err
		}
		return nil
	}), nil
}

func buildAnthropicMessages(messages []Message) (string, []anthropic.MessageParam) {
	messages = prepareAnthropicMessages(messages)

	var systemParts []string
	var out []anthropic.MessageParam
	var pendingDev string

	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			systemParts = append(systemParts, collectTextParts(msg.Parts))
		case RoleDeveloper:
			// Anthropic has no native developer role. Buffer the text and prepend
			// it into the next user turn wrapped in <developer> tags.
			pendingDev = collectTextParts(msg.Parts)
		case RoleUser:
			parts := msg.Parts
			if pendingDev != "" {
				parts = prependTextToParts(fmt.Sprintf("<developer>\n%s\n</developer>\n\n", pendingDev), parts)
				pendingDev = ""
			}
			blocks := buildAnthropicBlocks(parts, false)
			if len(blocks) > 0 {
				m := anthropic.NewUserMessage(blocks...)
				if msg.CacheAnchor {
					applyCacheControlToLastBlock(m.Content)
				}
				out = append(out, m)
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
	messages = prepareAnthropicMessages(messages)

	var systemParts []string
	var out []anthropic.BetaMessageParam
	var pendingDev string

	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			systemParts = append(systemParts, collectTextParts(msg.Parts))
		case RoleDeveloper:
			pendingDev = collectTextParts(msg.Parts)
		case RoleUser:
			parts := msg.Parts
			if pendingDev != "" {
				parts = prependTextToParts(fmt.Sprintf("<developer>\n%s\n</developer>\n\n", pendingDev), parts)
				pendingDev = ""
			}
			blocks := buildAnthropicBetaBlocks(parts, false)
			if len(blocks) > 0 {
				m := anthropic.NewBetaUserMessage(blocks...)
				if msg.CacheAnchor {
					applyBetaCacheControlToLastBlock(m.Content)
				}
				out = append(out, m)
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

// prependTextToParts prepends prefix to the first PartText part in parts,
// or inserts a new text part at the front if none exists.
func prependTextToParts(prefix string, parts []Part) []Part {
	for i, p := range parts {
		if p.Type == PartText {
			out := make([]Part, len(parts))
			copy(out, parts)
			out[i].Text = prefix + out[i].Text
			return out
		}
	}
	return append([]Part{{Type: PartText, Text: prefix}}, parts...)
}

func prepareAnthropicMessages(messages []Message) []Message {
	messages = sanitizeToolHistory(messages)
	if len(messages) == 0 {
		return nil
	}

	lastRole := messages[len(messages)-1].Role
	if lastRole != RoleAssistant {
		return messages
	}

	normalized := append([]Message(nil), messages...)
	// Anthropic treats a trailing assistant turn as response prefill. That is
	// deprecated and unsupported on newer Claude models, so convert assistant-
	// ended histories into a normal assistant->user continuation turn.
	normalized = append(normalized, UserText("Continue from the conversation state above."))
	return normalized
}

func buildAnthropicBlocks(parts []Part, allowToolUse bool) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case PartText:
			if allowToolUse && part.ReasoningEncryptedContent != "" {
				blocks = append(blocks, anthropic.NewThinkingBlock(part.ReasoningEncryptedContent, part.ReasoningContent))
			}
			if part.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(part.Text))
			}
		case PartImage:
			if part.ImageData != nil {
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfImage: &anthropic.ImageBlockParam{
						Source: anthropic.ImageBlockParamSourceUnion{
							OfBase64: &anthropic.Base64ImageSourceParam{
								Data:      part.ImageData.Base64,
								MediaType: anthropic.Base64ImageSourceMediaType(part.ImageData.MediaType),
							},
						},
					},
				})
				if part.ImagePath != "" {
					blocks = append(blocks, anthropic.NewTextBlock("[image saved at: "+part.ImagePath+"]"))
				}
			}
		case PartToolCall:
			if allowToolUse && part.ToolCall != nil {
				blocks = append(blocks, anthropic.NewToolUseBlock(part.ToolCall.ID, part.ToolCall.Arguments, part.ToolCall.Name))
			}
		case PartToolResult:
			if part.ToolResult != nil {
				blocks = append(blocks, toolResultBlock(part.ToolResult))
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
			if allowToolUse && part.ReasoningEncryptedContent != "" {
				blocks = append(blocks, anthropic.NewBetaThinkingBlock(part.ReasoningEncryptedContent, part.ReasoningContent))
			}
			if part.Text != "" {
				blocks = append(blocks, anthropic.NewBetaTextBlock(part.Text))
			}
		case PartImage:
			if part.ImageData != nil {
				blocks = append(blocks, anthropic.BetaContentBlockParamUnion{
					OfImage: &anthropic.BetaImageBlockParam{
						Source: anthropic.BetaImageBlockParamSourceUnion{
							OfBase64: &anthropic.BetaBase64ImageSourceParam{
								Data:      part.ImageData.Base64,
								MediaType: anthropic.BetaBase64ImageSourceMediaType(part.ImageData.MediaType),
							},
						},
					},
				})
				if part.ImagePath != "" {
					blocks = append(blocks, anthropic.NewBetaTextBlock("[image saved at: "+part.ImagePath+"]"))
				}
			}
		case PartToolCall:
			if allowToolUse && part.ToolCall != nil {
				blocks = append(blocks, anthropic.NewBetaToolUseBlock(part.ToolCall.ID, part.ToolCall.Arguments, part.ToolCall.Name))
			}
		case PartToolResult:
			if part.ToolResult != nil {
				blocks = append(blocks, betaToolResultBlock(part.ToolResult))
			}
		}
	}
	return blocks
}

func betaToolResultBlock(result *ToolResult) anthropic.BetaContentBlockParamUnion {
	contentBlocks := make([]anthropic.BetaToolResultBlockParamContentUnion, 0)

	for _, part := range toolResultContentParts(result) {
		switch part.Type {
		case ToolContentPartText:
			if part.Text == "" {
				continue
			}
			contentBlocks = append(contentBlocks, anthropic.BetaToolResultBlockParamContentUnion{
				OfText: &anthropic.BetaTextBlockParam{Text: part.Text},
			})
		case ToolContentPartImageData:
			mimeType, base64Data, ok := toolResultImageData(part)
			if !ok {
				continue
			}
			contentBlocks = append(contentBlocks, anthropic.BetaToolResultBlockParamContentUnion{
				OfImage: &anthropic.BetaImageBlockParam{
					Source: anthropic.BetaImageBlockParamSourceUnion{
						OfBase64: &anthropic.BetaBase64ImageSourceParam{
							Data:      base64Data,
							MediaType: anthropic.BetaBase64ImageSourceMediaType(mimeType),
						},
					},
				},
			})
		}
	}

	if len(contentBlocks) == 0 {
		textContent := toolResultTextContent(result)
		contentBlocks = append(contentBlocks, anthropic.BetaToolResultBlockParamContentUnion{
			OfText: &anthropic.BetaTextBlockParam{Text: textContent},
		})
	}

	block := anthropic.BetaToolResultBlockParam{
		ToolUseID: result.ID,
		IsError:   anthropic.Bool(result.IsError),
		Content:   contentBlocks,
	}
	return anthropic.BetaContentBlockParamUnion{OfToolResult: &block}
}

// toolResultBlock creates a non-beta tool result block with structured image support.
func toolResultBlock(result *ToolResult) anthropic.ContentBlockParamUnion {
	contentBlocks := make([]anthropic.ToolResultBlockParamContentUnion, 0)

	for _, part := range toolResultContentParts(result) {
		switch part.Type {
		case ToolContentPartText:
			if part.Text == "" {
				continue
			}
			contentBlocks = append(contentBlocks, anthropic.ToolResultBlockParamContentUnion{
				OfText: &anthropic.TextBlockParam{Text: part.Text},
			})
		case ToolContentPartImageData:
			mimeType, base64Data, ok := toolResultImageData(part)
			if !ok {
				continue
			}
			contentBlocks = append(contentBlocks, anthropic.ToolResultBlockParamContentUnion{
				OfImage: &anthropic.ImageBlockParam{
					Source: anthropic.ImageBlockParamSourceUnion{
						OfBase64: &anthropic.Base64ImageSourceParam{
							Data:      base64Data,
							MediaType: anthropic.Base64ImageSourceMediaType(mimeType),
						},
					},
				},
			})
		}
	}

	if len(contentBlocks) == 0 {
		textContent := toolResultTextContent(result)
		contentBlocks = append(contentBlocks, anthropic.ToolResultBlockParamContentUnion{
			OfText: &anthropic.TextBlockParam{Text: textContent},
		})
	}

	block := anthropic.ToolResultBlockParam{
		ToolUseID: result.ID,
		IsError:   anthropic.Bool(result.IsError),
		Content:   contentBlocks,
	}
	return anthropic.ContentBlockParamUnion{OfToolResult: &block}
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
	if len(tools) > 0 && tools[len(tools)-1].OfTool != nil {
		tools[len(tools)-1].OfTool.CacheControl = anthropic.NewCacheControlEphemeralParam()
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
	if len(tools) > 0 && tools[len(tools)-1].OfTool != nil {
		tools[len(tools)-1].OfTool.CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
	}
	return tools
}

// applyLastMessageCacheControl marks the last content block of the last message
// for caching. This enables incremental conversation caching: each turn, the
// prior conversation becomes a cache hit and only the new turn is processed fresh.
func applyLastMessageCacheControl(messages []anthropic.MessageParam) {
	if len(messages) == 0 {
		return
	}
	last := &messages[len(messages)-1]
	if len(last.Content) == 0 {
		return
	}
	applyCacheControlToLastBlock(last.Content)
}

// applyCacheControlToLastBlock applies cache_control: ephemeral to the last block
// in a slice of Anthropic content blocks. Used for both the rolling per-turn
// breakpoint and the stable summary anchor.
func applyCacheControlToLastBlock(blocks []anthropic.ContentBlockParamUnion) {
	if len(blocks) == 0 {
		return
	}
	cc := anthropic.NewCacheControlEphemeralParam()
	block := &blocks[len(blocks)-1]
	switch {
	case block.OfText != nil:
		block.OfText.CacheControl = cc
	case block.OfImage != nil:
		block.OfImage.CacheControl = cc
	case block.OfDocument != nil:
		block.OfDocument.CacheControl = cc
	case block.OfToolResult != nil:
		block.OfToolResult.CacheControl = cc
	}
}

// applyBetaLastMessageCacheControl marks the last content block of the last beta
// message for caching.
func applyBetaLastMessageCacheControl(messages []anthropic.BetaMessageParam) {
	if len(messages) == 0 {
		return
	}
	last := &messages[len(messages)-1]
	if len(last.Content) == 0 {
		return
	}
	applyBetaCacheControlToLastBlock(last.Content)
}

// applyBetaCacheControlToLastBlock applies cache_control: ephemeral to the last
// block in a slice of Anthropic beta content blocks.
func applyBetaCacheControlToLastBlock(blocks []anthropic.BetaContentBlockParamUnion) {
	if len(blocks) == 0 {
		return
	}
	cc := anthropic.NewBetaCacheControlEphemeralParam()
	block := &blocks[len(blocks)-1]
	switch {
	case block.OfText != nil:
		block.OfText.CacheControl = cc
	case block.OfImage != nil:
		block.OfImage.CacheControl = cc
	case block.OfDocument != nil:
		block.OfDocument.CacheControl = cc
	case block.OfToolResult != nil:
		block.OfToolResult.CacheControl = cc
	}
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

func handleAnthropicStartBlockContent(send eventSender, block any, index int64, accumulator *toolCallAccumulator) error {
	switch variant := block.(type) {
	case anthropic.TextBlock:
		if variant.Text != "" {
			if err := send.Send(Event{Type: EventTextDelta, Text: variant.Text}); err != nil {
				return err
			}
		}
	case anthropic.ThinkingBlock:
		if err := emitReasoningDelta(send, variant.Thinking, variant.Signature); err != nil {
			return err
		}
	case anthropic.ToolUseBlock:
		accumulator.Start(index, ToolCall{
			ID:        variant.ID,
			Name:      variant.Name,
			Arguments: toolInputToRaw(variant.Input),
		})
	}
	return nil
}

func handleAnthropicBetaStartBlockContent(send eventSender, block any, index int64, accumulator *toolCallAccumulator) error {
	switch variant := block.(type) {
	case anthropic.BetaTextBlock:
		if variant.Text != "" {
			if err := send.Send(Event{Type: EventTextDelta, Text: variant.Text}); err != nil {
				return err
			}
		}
	case anthropic.BetaThinkingBlock:
		if err := emitReasoningDelta(send, variant.Thinking, variant.Signature); err != nil {
			return err
		}
	case anthropic.BetaToolUseBlock:
		accumulator.Start(index, ToolCall{
			ID:        variant.ID,
			Name:      variant.Name,
			Arguments: toolInputToRaw(variant.Input),
		})
	}
	return nil
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

func emitReasoningDelta(send eventSender, text, encrypted string) error {
	if text == "" && encrypted == "" {
		return nil
	}
	return send.Send(Event{
		Type:                      EventReasoningDelta,
		Text:                      text,
		ReasoningEncryptedContent: encrypted,
	})
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
