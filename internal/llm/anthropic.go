package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
	"github.com/samsaffron/term-llm/internal/credentials"
	"golang.org/x/term"
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
	AnthropicCredAuto     = "auto"      // Default cascade: api_key → env → oauth_env → oauth → interactive
	AnthropicCredAPIKey   = "api_key"   // Force: explicit api_key from config only
	AnthropicCredEnv      = "env"       // Force: ANTHROPIC_API_KEY env var only
	AnthropicCredOAuthEnv = "oauth_env" // Force: CLAUDE_CODE_OAUTH_TOKEN env var only
	AnthropicCredOAuth    = "oauth"     // Force: saved OAuth token or interactive setup
)

// AnthropicProvider implements Provider using the Anthropic API.
type AnthropicProvider struct {
	client         *anthropic.Client
	model          string
	thinkingBudget int64  // 0 = disabled, >0 = enabled with budget
	useAdaptive    bool   // true = adaptive thinking (-thinking on 4.6 models)
	credential     string // "api_key", "env", "oauth_env", or "oauth"
}

// isAdaptiveModel returns true for Claude 4.6 models that support adaptive thinking.
func isAdaptiveModel(model string) bool {
	return strings.HasPrefix(model, "claude-sonnet-4-6") || strings.HasPrefix(model, "claude-opus-4-6")
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

// oauthBetaHeader is the beta header required to enable OAuth authentication.
const oauthBetaHeader = "oauth-2025-04-20"

// newOAuthClient creates an Anthropic client configured for OAuth Bearer token auth.
// OAuth requires the anthropic-beta: oauth-2025-04-20 header on every request.
func newOAuthClient(token string) anthropic.Client {
	return anthropic.NewClient(
		option.WithAuthToken(token),
		option.WithHeader("anthropic-beta", oauthBetaHeader),
	)
}

// NewAnthropicProvider creates a new Anthropic provider.
// The credentialMode parameter controls which authentication method is used:
//   - "" or "auto": try the full cascade (api_key → env → oauth_env → oauth → interactive)
//   - "api_key":    use only the explicit apiKey parameter
//   - "env":        use only the ANTHROPIC_API_KEY environment variable
//   - "oauth_env":  use only the CLAUDE_CODE_OAUTH_TOKEN environment variable
//   - "oauth":      use only saved OAuth token or interactive setup
func NewAnthropicProvider(apiKey, model, credentialMode string) (*AnthropicProvider, error) {
	actualModel, thinkingBudget, adaptive := parseModelThinking(model)

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

	case AnthropicCredOAuthEnv:
		envToken := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
		if envToken == "" {
			return nil, fmt.Errorf("credentials mode %q requires CLAUDE_CODE_OAUTH_TOKEN environment variable", credentialMode)
		}
		return mkProvider(newOAuthClient(envToken), "oauth_env"), nil

	case AnthropicCredOAuth:
		return newAnthropicOAuthProvider(actualModel, thinkingBudget, adaptive)

	case AnthropicCredAuto:
		// Fall through to the cascade below.

	default:
		return nil, fmt.Errorf("unknown Anthropic credentials mode: %q (valid: auto, api_key, env, oauth_env, oauth)", credentialMode)
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

	// 3. CLAUDE_CODE_OAUTH_TOKEN environment variable
	if envToken := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); envToken != "" {
		return mkProvider(newOAuthClient(envToken), "oauth_env"), nil
	}

	// 4. Saved OAuth token from local storage
	if creds, err := credentials.GetAnthropicOAuthCredentials(); err == nil {
		return mkProvider(newOAuthClient(creds.AccessToken), "oauth"), nil
	}

	// 5. Interactive: prompt user to run `claude setup-token` and paste the token
	return newAnthropicOAuthProvider(actualModel, thinkingBudget, adaptive)
}

// newAnthropicOAuthProvider creates an Anthropic provider using saved OAuth credentials
// or interactively prompts the user to set up a new token.
func newAnthropicOAuthProvider(model string, thinkingBudget int64, adaptive bool) (*AnthropicProvider, error) {
	// Try saved OAuth token first
	if creds, err := credentials.GetAnthropicOAuthCredentials(); err == nil {
		client := newOAuthClient(creds.AccessToken)
		return &AnthropicProvider{
			client:         &client,
			model:          model,
			thinkingBudget: thinkingBudget,
			useAdaptive:    adaptive,
			credential:     "oauth",
		}, nil
	}

	// Interactive: prompt user to run `claude setup-token` and paste the token
	token, err := promptForAnthropicOAuth()
	if err != nil {
		return nil, err
	}

	// Validate the token works before saving
	testClient := newOAuthClient(token)
	if err := validateAnthropicToken(&testClient); err != nil {
		return nil, fmt.Errorf("token validation failed: %w", err)
	}

	// Save the validated token for future use
	if err := credentials.SaveAnthropicOAuthCredentials(&credentials.AnthropicOAuthCredentials{
		AccessToken: token,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save OAuth token: %v\n", err)
	}

	fmt.Fprintln(os.Stderr, "Token validated and saved successfully.")

	return &AnthropicProvider{
		client:         &testClient,
		model:          model,
		thinkingBudget: thinkingBudget,
		useAdaptive:    adaptive,
		credential:     "oauth",
	}, nil
}

// validateAnthropicToken checks that a token works by making a lightweight API call.
func validateAnthropicToken(client *anthropic.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := client.Models.List(ctx, anthropic.ModelListParams{})
	if err != nil {
		return fmt.Errorf("invalid or expired token (API returned error): %w", err)
	}
	return nil
}

// promptForAnthropicOAuth asks the user to run `claude setup-token` and paste the resulting token.
// Returns an error if running in a non-interactive context (e.g., scripts, CI).
func promptForAnthropicOAuth() (string, error) {
	// Check if stdin is a terminal - if not, we can't do interactive auth
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("Anthropic authentication required but running in non-interactive mode.\n" +
			"Set ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, or run interactively to authenticate")
	}

	fmt.Fprintln(os.Stderr, "No Anthropic credentials found.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "To authenticate, run this in another terminal:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  claude setup-token")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "This requires the Claude Code CLI and a Claude subscription (Pro/Max).")
	fmt.Fprintln(os.Stderr, "Copy the token it generates and paste it below.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, "Paste token: ")

	// Read token with echo disabled so it isn't visible in terminal or logs
	tokenBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr) // newline after hidden input
	if err != nil {
		return "", fmt.Errorf("failed to read token: %w", err)
	}

	// Strip all whitespace (spaces, tabs, newlines) that may sneak in during paste
	token := strings.Join(strings.Fields(string(tokenBytes)), "")
	if token == "" {
		return "", fmt.Errorf("empty token provided")
	}

	return token, nil
}

func (p *AnthropicProvider) Name() string {
	if p.useAdaptive {
		return fmt.Sprintf("Anthropic (%s, adaptive)", p.model)
	}
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
		NativeWebSearch:    true,
		NativeWebFetch:     true,
		ToolCalls:          true,
		SupportsToolChoice: true,
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
			adaptive := anthropic.NewThinkingConfigAdaptiveParam()
			params.Thinking = anthropic.ThinkingConfigParamUnion{
				OfAdaptive: &adaptive,
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
				case anthropic.ThinkingDelta:
					emitReasoningDelta(events, delta.Thinking, "")
				case anthropic.SignatureDelta:
					emitReasoningDelta(events, "", delta.Signature)
				}
			case anthropic.ContentBlockStartEvent:
				handleAnthropicStartBlockContent(variant.ContentBlock.AsAny(), variant.Index, accumulator, events)
			case anthropic.ContentBlockStopEvent:
				if toolCall, ok := accumulator.Finish(variant.Index); ok {
					events <- Event{Type: EventToolCall, Tool: &toolCall}
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
			events <- Event{Type: EventUsage, Use: lastUsage}
		}
		events <- Event{Type: EventDone}
		return nil
	}), nil
}

func (p *AnthropicProvider) streamWithSearch(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
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

		params := anthropic.BetaMessageNewParams{
			Model:     anthropic.Model(chooseModel(req.Model, p.model)),
			MaxTokens: maxTokens(req.MaxOutputTokens, 4096),
			Betas:     []anthropic.AnthropicBeta{"web-search-2025-03-05", "web-fetch-2025-09-10"},
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
			adaptive := anthropic.NewBetaThinkingConfigAdaptiveParam()
			params.Thinking = anthropic.BetaThinkingConfigParamUnion{
				OfAdaptive: &adaptive,
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
							events <- Event{Type: EventToolExecEnd, ToolName: currentServerTool, ToolSuccess: true}
							currentServerTool = ""
						}
						events <- Event{Type: EventTextDelta, Text: delta.Text}
					}
				case anthropic.BetaThinkingDelta:
					emitReasoningDelta(events, delta.Thinking, "")
				case anthropic.BetaSignatureDelta:
					emitReasoningDelta(events, "", delta.Signature)
				}
			case anthropic.BetaRawContentBlockStartEvent:
				blockType := variant.ContentBlock.Type
				if blockType == "server_tool_use" {
					// Server tool (web_search, etc.) is starting
					serverTool := variant.ContentBlock.AsServerToolUse()
					toolName := string(serverTool.Name)
					currentServerTool = toolName
					events <- Event{Type: EventToolExecStart, ToolName: toolName}
				} else {
					handleAnthropicBetaStartBlockContent(variant.ContentBlock.AsAny(), variant.Index, accumulator, events)
				}
			case anthropic.BetaRawContentBlockStopEvent:
				if toolCall, ok := accumulator.Finish(variant.Index); ok {
					events <- Event{Type: EventToolCall, Tool: &toolCall}
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
		if lastUsage != nil {
			events <- Event{Type: EventUsage, Use: lastUsage}
		}
		events <- Event{Type: EventDone}
		return nil
	}), nil
}

func buildAnthropicMessages(messages []Message) (string, []anthropic.MessageParam) {
	messages = sanitizeToolHistory(messages)

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
	messages = sanitizeToolHistory(messages)

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
	cc := anthropic.NewCacheControlEphemeralParam()
	block := &last.Content[len(last.Content)-1]
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
	cc := anthropic.NewBetaCacheControlEphemeralParam()
	block := &last.Content[len(last.Content)-1]
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

func handleAnthropicStartBlockContent(block any, index int64, accumulator *toolCallAccumulator, events chan<- Event) {
	switch variant := block.(type) {
	case anthropic.TextBlock:
		if variant.Text != "" {
			events <- Event{Type: EventTextDelta, Text: variant.Text}
		}
	case anthropic.ThinkingBlock:
		emitReasoningDelta(events, variant.Thinking, variant.Signature)
	case anthropic.ToolUseBlock:
		accumulator.Start(index, ToolCall{
			ID:        variant.ID,
			Name:      variant.Name,
			Arguments: toolInputToRaw(variant.Input),
		})
	}
}

func handleAnthropicBetaStartBlockContent(block any, index int64, accumulator *toolCallAccumulator, events chan<- Event) {
	switch variant := block.(type) {
	case anthropic.BetaTextBlock:
		if variant.Text != "" {
			events <- Event{Type: EventTextDelta, Text: variant.Text}
		}
	case anthropic.BetaThinkingBlock:
		emitReasoningDelta(events, variant.Thinking, variant.Signature)
	case anthropic.BetaToolUseBlock:
		accumulator.Start(index, ToolCall{
			ID:        variant.ID,
			Name:      variant.Name,
			Arguments: toolInputToRaw(variant.Input),
		})
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

func emitReasoningDelta(events chan<- Event, text, encrypted string) {
	if text == "" && encrypted == "" {
		return
	}
	events <- Event{
		Type:                      EventReasoningDelta,
		Text:                      text,
		ReasoningEncryptedContent: encrypted,
	}
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
