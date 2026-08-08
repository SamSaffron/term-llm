package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
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
	client          *anthropic.Client
	model           string
	thinkingBudget  int64  // 0 = disabled, >0 = enabled with budget
	useAdaptive     bool   // true = adaptive thinking (-thinking on adaptive-capable models)
	use1m           bool   // true = 1M token context window (-1m suffix)
	reasoningEffort string // default output_config.effort from model suffix, if any
	credential      string // "api_key" or "env"
}

// isAdaptiveModel returns true for Claude models that support adaptive thinking.
func isAdaptiveModel(model string) bool {
	return strings.HasPrefix(model, "claude-fable-5") ||
		strings.HasPrefix(model, "claude-opus-4-8") ||
		strings.HasPrefix(model, "claude-sonnet-4-6") ||
		strings.HasPrefix(model, "claude-opus-4-6") ||
		strings.HasPrefix(model, "claude-opus-4-7")
}

// parseModelThinking extracts -thinking suffix from model name.
// For adaptive-capable models, -thinking uses adaptive thinking
// (budget_tokens is deprecated). For older models, -thinking uses
// budget_tokens as before.
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

// NewAnthropicProvider creates a new Anthropic provider using Anthropic's default API endpoint.
func NewAnthropicProvider(apiKey, model, credentialMode string) (*AnthropicProvider, error) {
	return NewAnthropicProviderWithBaseURL(apiKey, model, credentialMode, "")
}

// NewAnthropicProviderWithBaseURL creates a new Anthropic provider.
// The credentialMode parameter controls which authentication method is used:
//   - "" or "auto": try the cascade (api_key → env)
//   - "api_key":    use only the explicit apiKey parameter
//   - "env":        use only the ANTHROPIC_API_KEY environment variable
func NewAnthropicProviderWithBaseURL(apiKey, model, credentialMode, baseURL string) (*AnthropicProvider, error) {
	// Strip provider-aware reasoning effort first, then -thinking (may leave -1m), then -1m.
	// This means claude-sonnet-4-6-1m-thinking works correctly:
	//   step 1: strip effort   -> "claude-sonnet-4-6-1m-thinking"
	//   step 2: strip thinking -> "claude-sonnet-4-6-1m", adaptive=true
	//   step 3: strip -1m      -> "claude-sonnet-4-6",    use1m=true
	baseModel, reasoningEffort := BaseModelAndEffortForProvider("anthropic", model)
	if baseModel == "" {
		baseModel = model
	}
	afterThinking, thinkingBudget, adaptive := parseModelThinking(baseModel)
	actualModel, use1m := parseModel1m(afterThinking)

	// Normalize empty credential mode to "auto"
	if credentialMode == "" {
		credentialMode = AnthropicCredAuto
	}

	baseURL = strings.TrimSpace(baseURL)
	if baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil {
			return nil, fmt.Errorf("invalid Anthropic base_url %q: %w", baseURL, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("invalid Anthropic base_url %q: must include scheme and host", baseURL)
		}
	}
	mkClient := func(key string) anthropic.Client {
		opts := []option.RequestOption{option.WithAPIKey(key)}
		if baseURL != "" {
			opts = append(opts, option.WithBaseURL(baseURL))
		}
		return anthropic.NewClient(opts...)
	}

	mkProvider := func(client anthropic.Client, cred string) *AnthropicProvider {
		return &AnthropicProvider{
			client:          &client,
			model:           actualModel,
			thinkingBudget:  thinkingBudget,
			useAdaptive:     adaptive,
			use1m:           use1m,
			reasoningEffort: reasoningEffort,
			credential:      cred,
		}
	}

	// When a specific mode is forced, only try that one source.
	switch credentialMode {
	case AnthropicCredAPIKey:
		if apiKey == "" {
			return nil, fmt.Errorf("credentials mode %q requires an explicit api_key in provider config", credentialMode)
		}
		return mkProvider(mkClient(apiKey), "api_key"), nil

	case AnthropicCredEnv:
		envKey := os.Getenv("ANTHROPIC_API_KEY")
		if envKey == "" {
			return nil, fmt.Errorf("credentials mode %q requires ANTHROPIC_API_KEY environment variable", credentialMode)
		}
		return mkProvider(mkClient(envKey), "env"), nil

	case AnthropicCredAuto:
		// Fall through to the cascade below.

	default:
		return nil, fmt.Errorf("unknown Anthropic credentials mode: %q (valid: auto, api_key, env)", credentialMode)
	}

	// Auto mode: full credential cascade.

	// 1. Explicit API key provided (from config)
	if apiKey != "" {
		return mkProvider(mkClient(apiKey), "api_key"), nil
	}

	// 2. ANTHROPIC_API_KEY environment variable
	if envKey := os.Getenv("ANTHROPIC_API_KEY"); envKey != "" {
		return mkProvider(mkClient(envKey), "env"), nil
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
	model, _ := p.requestModelAndEffort(req)
	req.MaxOutputTokens = ClampOutputTokens(req.MaxOutputTokens, model)
	if req.Search {
		return p.streamWithSearch(ctx, req)
	}
	return p.streamStandard(ctx, req)
}

func (p *AnthropicProvider) requestModelAndEffort(req Request) (model string, effort string) {
	model = chooseModel(req.Model, p.model)
	effort = strings.TrimSpace(p.reasoningEffort)
	if base, suffix := BaseModelAndEffortForProvider("anthropic", model); base != "" {
		model = base
		if suffix != "" {
			effort = suffix
		}
	}
	if reqEffort := strings.TrimSpace(req.ReasoningEffort); reqEffort != "" {
		effort = reqEffort
	}
	return model, effort
}

func (p *AnthropicProvider) streamStandard(ctx context.Context, req Request) (Stream, error) {
	model, reasoningEffort := p.requestModelAndEffort(req)
	return p.streamStandardForModel(ctx, req, model, reasoningEffort, p.thinkingBudget, p.useAdaptive, true)
}

func (p *AnthropicProvider) streamStandardForModel(ctx context.Context, req Request, model, reasoningEffort string, thinkingBudget int64, useAdaptive, includeOutputEffort bool) (Stream, error) {
	if req.MaxOutputTokens > 0 && thinkingBudget >= int64(req.MaxOutputTokens) {
		thinkingBudget = int64(req.MaxOutputTokens - 1)
	}
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		system, messages := buildAnthropicMessages(req.Messages)
		applyLastMessageCacheControl(messages)
		accumulator := newToolCallAccumulator()

		params := anthropic.MessageNewParams{
			Model:     anthropic.Model(model),
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
			if thinkingBudget == 0 && !useAdaptive {
				params.ToolChoice = buildAnthropicToolChoice(req.ToolChoice, req.ParallelToolCalls)
			}
		}

		if useAdaptive {
			params.MaxTokens = maxTokens(req.MaxOutputTokens, 16000)
			params.Thinking = anthropic.ThinkingConfigParamUnion{
				OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
			}
		} else if thinkingBudget > 0 {
			fallbackMaxTokens := 16000
			if thinkingBudget >= int64(fallbackMaxTokens) {
				fallbackMaxTokens = int(thinkingBudget + 1)
			}
			params.MaxTokens = maxTokens(req.MaxOutputTokens, fallbackMaxTokens)
			params.Thinking = anthropic.ThinkingConfigParamUnion{
				OfEnabled: &anthropic.ThinkingConfigEnabledParam{
					BudgetTokens: thinkingBudget,
				},
			}
			if includeOutputEffort {
				params.OutputConfig = anthropic.OutputConfigParam{
					Effort: anthropic.OutputConfigEffortMax,
				}
			}
		}
		if eff := strings.TrimSpace(reasoningEffort); eff != "" {
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
		sawMessageStop := false
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
			case anthropic.MessageStopEvent:
				sawMessageStop = true
			}
		}
		if err := validateAnthropicStreamEnd(stream.Err(), sawMessageStop); err != nil {
			return err
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

func validateAnthropicStreamEnd(streamErr error, sawMessageStop bool) error {
	// The Anthropic HTTP decoder reports a clean terminal read as nil, while
	// the shared Bedrock eventstream decoder reports io.EOF after message_stop.
	// Normalize that transport difference only after validating the protocol's
	// required terminal event so a truncated Bedrock stream cannot become Done.
	if streamErr != nil && !errors.Is(streamErr, io.EOF) {
		return fmt.Errorf("anthropic streaming error: %w", streamErr)
	}
	if !sawMessageStop {
		return &StreamIncompleteError{Transport: "Anthropic SSE", Terminal: "message_stop"}
	}
	return nil
}

func finishAnthropicServerToolFailure(send eventSender, toolName string, streamErr error) error {
	if toolName == "" {
		return streamErr
	}
	if sendErr := send.Send(Event{Type: EventToolExecEnd, ToolName: toolName, ToolSuccess: false}); sendErr != nil {
		return errors.Join(streamErr, sendErr)
	}
	return streamErr
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
		model, reasoningEffort := p.requestModelAndEffort(req)
		params := anthropic.BetaMessageNewParams{
			Model:     anthropic.Model(model),
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
		if eff := strings.TrimSpace(reasoningEffort); eff != "" {
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
		sawMessageStop := false

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
			case anthropic.BetaRawMessageStopEvent:
				sawMessageStop = true
			}
		}
		if err := validateAnthropicStreamEnd(stream.Err(), sawMessageStop); err != nil {
			return finishAnthropicServerToolFailure(send, currentServerTool, err)
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

type anthropicPartConstructors[T any] struct {
	thinking   func(string, string) T
	text       func(string) T
	image      func(*ToolImageData) T
	toolUse    func(*ToolCall) T
	toolResult func(*ToolResult) T
}

func buildAnthropicParts[T any](parts []Part, allowToolUse bool, constructors anthropicPartConstructors[T]) []T {
	blocks := make([]T, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case PartText, PartFile:
			if allowToolUse && part.ReasoningEncryptedContent != "" {
				blocks = append(blocks, constructors.thinking(part.ReasoningEncryptedContent, part.ReasoningContent))
			}
			if part.Text != "" {
				blocks = append(blocks, constructors.text(part.Text))
			}
		case PartImage:
			if part.ImageData != nil && strings.TrimSpace(part.ImageData.Base64) != "" {
				blocks = append(blocks, constructors.image(part.ImageData))
				if part.ImagePath != "" {
					blocks = append(blocks, constructors.text("[image saved at: "+part.ImagePath+"]"))
				}
			}
		case PartToolCall:
			if allowToolUse && part.ToolCall != nil {
				blocks = append(blocks, constructors.toolUse(part.ToolCall))
			}
		case PartToolResult:
			if part.ToolResult != nil {
				blocks = append(blocks, constructors.toolResult(part.ToolResult))
			}
		}
	}
	return blocks
}

func newAnthropicImageBlock(image *ToolImageData) anthropic.ContentBlockParamUnion {
	return anthropic.ContentBlockParamUnion{OfImage: &anthropic.ImageBlockParam{Source: anthropic.ImageBlockParamSourceUnion{OfBase64: &anthropic.Base64ImageSourceParam{Data: image.Base64, MediaType: anthropic.Base64ImageSourceMediaType(image.MediaType)}}}}
}

func newAnthropicBetaImageBlock(image *ToolImageData) anthropic.BetaContentBlockParamUnion {
	return anthropic.BetaContentBlockParamUnion{OfImage: &anthropic.BetaImageBlockParam{Source: anthropic.BetaImageBlockParamSourceUnion{OfBase64: &anthropic.BetaBase64ImageSourceParam{Data: image.Base64, MediaType: anthropic.BetaBase64ImageSourceMediaType(image.MediaType)}}}}
}

func buildAnthropicBlocks(parts []Part, allowToolUse bool) []anthropic.ContentBlockParamUnion {
	return buildAnthropicParts(parts, allowToolUse, anthropicPartConstructors[anthropic.ContentBlockParamUnion]{
		thinking: anthropic.NewThinkingBlock,
		text:     anthropic.NewTextBlock,
		image:    newAnthropicImageBlock,
		toolUse: func(call *ToolCall) anthropic.ContentBlockParamUnion {
			return anthropic.NewToolUseBlock(call.ID, call.Arguments, call.Name)
		},
		toolResult: toolResultBlock,
	})
}

func buildAnthropicBetaBlocks(parts []Part, allowToolUse bool) []anthropic.BetaContentBlockParamUnion {
	return buildAnthropicParts(parts, allowToolUse, anthropicPartConstructors[anthropic.BetaContentBlockParamUnion]{
		thinking: anthropic.NewBetaThinkingBlock,
		text:     anthropic.NewBetaTextBlock,
		image:    newAnthropicBetaImageBlock,
		toolUse: func(call *ToolCall) anthropic.BetaContentBlockParamUnion {
			return anthropic.NewBetaToolUseBlock(call.ID, call.Arguments, call.Name)
		},
		toolResult: betaToolResultBlock,
	})
}

type anthropicResultContent struct {
	text       string
	mimeType   string
	base64Data string
}

func normalizedAnthropicResultContent(result *ToolResult) []anthropicResultContent {
	var content []anthropicResultContent
	for _, part := range toolResultContentParts(result) {
		switch part.Type {
		case ToolContentPartText:
			if part.Text != "" {
				content = append(content, anthropicResultContent{text: part.Text})
			}
		case ToolContentPartImageData:
			mimeType, base64Data, ok := toolResultImageData(part)
			if ok {
				content = append(content, anthropicResultContent{mimeType: mimeType, base64Data: base64Data})
			}
		}
	}
	if len(content) == 0 {
		content = append(content, anthropicResultContent{text: toolResultTextContent(result)})
	}
	return content
}

func mapAnthropicResultContent[T any](result *ToolResult, text func(string) T, image func(string, string) T) []T {
	content := normalizedAnthropicResultContent(result)
	blocks := make([]T, 0, len(content))
	for _, part := range content {
		if part.text != "" {
			blocks = append(blocks, text(part.text))
		} else {
			blocks = append(blocks, image(part.mimeType, part.base64Data))
		}
	}
	return blocks
}

func newBetaResultText(text string) anthropic.BetaToolResultBlockParamContentUnion {
	return anthropic.BetaToolResultBlockParamContentUnion{OfText: &anthropic.BetaTextBlockParam{Text: text}}
}

func newBetaResultImage(mimeType, data string) anthropic.BetaToolResultBlockParamContentUnion {
	return anthropic.BetaToolResultBlockParamContentUnion{OfImage: &anthropic.BetaImageBlockParam{Source: anthropic.BetaImageBlockParamSourceUnion{OfBase64: &anthropic.BetaBase64ImageSourceParam{Data: data, MediaType: anthropic.BetaBase64ImageSourceMediaType(mimeType)}}}}
}

func newResultText(text string) anthropic.ToolResultBlockParamContentUnion {
	return anthropic.ToolResultBlockParamContentUnion{OfText: &anthropic.TextBlockParam{Text: text}}
}

func newResultImage(mimeType, data string) anthropic.ToolResultBlockParamContentUnion {
	return anthropic.ToolResultBlockParamContentUnion{OfImage: &anthropic.ImageBlockParam{Source: anthropic.ImageBlockParamSourceUnion{OfBase64: &anthropic.Base64ImageSourceParam{Data: data, MediaType: anthropic.Base64ImageSourceMediaType(mimeType)}}}}
}

func betaToolResultBlock(result *ToolResult) anthropic.BetaContentBlockParamUnion {
	content := mapAnthropicResultContent(result, newBetaResultText, newBetaResultImage)
	block := anthropic.BetaToolResultBlockParam{ToolUseID: result.ID, IsError: anthropic.Bool(result.IsError), Content: content}
	return anthropic.BetaContentBlockParamUnion{OfToolResult: &block}
}

// toolResultBlock creates a non-beta tool result block with structured image support.
func toolResultBlock(result *ToolResult) anthropic.ContentBlockParamUnion {
	content := mapAnthropicResultContent(result, newResultText, newResultImage)
	block := anthropic.ToolResultBlockParam{ToolUseID: result.ID, IsError: anthropic.Bool(result.IsError), Content: content}
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

func emitAnthropicTextStart(send eventSender, text string) error {
	if text == "" {
		return nil
	}
	return send.Send(Event{Type: EventTextDelta, Text: text})
}

func emitAnthropicThinkingStart(send eventSender, thinking, signature string) error {
	return emitReasoningDelta(send, thinking, signature)
}

func recordAnthropicToolStart(index int64, accumulator *toolCallAccumulator, call ToolCall) error {
	accumulator.Start(index, call)
	return nil
}

func handleAnthropicStartBlockContent(send eventSender, block any, index int64, accumulator *toolCallAccumulator) error {
	switch variant := block.(type) {
	case anthropic.TextBlock:
		return emitAnthropicTextStart(send, variant.Text)
	case anthropic.ThinkingBlock:
		return emitAnthropicThinkingStart(send, variant.Thinking, variant.Signature)
	case anthropic.ToolUseBlock:
		call := ToolCall{ID: variant.ID, Name: variant.Name, Arguments: toolInputToRaw(variant.Input)}
		return recordAnthropicToolStart(index, accumulator, call)
	}
	return nil
}

func handleAnthropicBetaStartBlockContent(send eventSender, block any, index int64, accumulator *toolCallAccumulator) error {
	switch variant := block.(type) {
	case anthropic.BetaTextBlock:
		return emitAnthropicTextStart(send, variant.Text)
	case anthropic.BetaThinkingBlock:
		return emitAnthropicThinkingStart(send, variant.Thinking, variant.Signature)
	case anthropic.BetaToolUseBlock:
		call := ToolCall{ID: variant.ID, Name: variant.Name, Arguments: toolInputToRaw(variant.Input)}
		return recordAnthropicToolStart(index, accumulator, call)
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
	kind := ReasoningKindRaw
	if text == "" && encrypted != "" {
		kind = ReasoningKindEncrypted
	}
	return send.Send(Event{
		Type:                      EventReasoningDelta,
		Text:                      text,
		ReasoningKind:             kind,
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
