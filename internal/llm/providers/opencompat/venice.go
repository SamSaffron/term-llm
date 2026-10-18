package opencompat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

const veniceBaseURL = "https://api.venice.ai/api/v1"

type VeniceProvider struct {
	*OpenAICompatProvider
}

func NewVeniceProvider(apiKey, model string) *VeniceProvider {
	apiKey = config.NormalizeVeniceAPIKey(apiKey)
	if model == "" {
		model = config.DefaultProviderModel("venice")
	}
	return &VeniceProvider{OpenAICompatProvider: NewOpenAICompatProvider(veniceBaseURL, apiKey, model, "Venice")}
}

func (p *VeniceProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    true,
		NativeWebFetch:     false,
		ToolCalls:          true,
		SupportsToolChoice: true,
	}
}

func (p *VeniceProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	models, err := p.OpenAICompatProvider.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	for i := range models {
		// Venice's /models response includes context_length. Keep the live value and
		// fall back only to a previously fetched Venice cache entry for compatible
		// responses that omit it; do not mix in curated/static limits.
		if models[i].InputLimit == 0 {
			models[i].InputLimit = veniceCachedInputLimit(models[i].ID)
		}
	}
	RefreshVeniceCacheSync(models)
	return models, nil
}

func (p *VeniceProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	req.MaxOutputTokens = ClampOutputTokens(req.MaxOutputTokens, chooseModel(req.Model, p.model))
	messages := buildCompatMessages(req.Messages)
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}

	tools, err := buildCompatTools(req.Tools)
	if err != nil {
		return nil, err
	}

	// Effort precedence: req.ReasoningEffort wins over model suffix, which wins over provider-level effort.
	baseModel := chooseModel(req.Model, p.model)
	strippedModel, reqEffort := ParseModelEffort(baseModel)
	effort := p.effort
	if reqEffort != "" {
		effort = reqEffort
		baseModel = strippedModel
	}
	if v := strings.TrimSpace(req.ReasoningEffort); v != "" {
		effort = v
	}

	model, veniceParams := buildVeniceModelAndParams(baseModel, req.Search)
	if isVeniceClaudeModel(model) {
		applyVeniceClaudeCacheBreakpoint(messages)
	}
	chatReq := oaiChatRequest{
		Model:            model,
		Messages:         messages,
		Tools:            tools,
		Stream:           true,
		ReasoningEffort:  effort,
		VeniceParameters: veniceParams,
		// Routing hint: requests with the same key are more likely to reach a server with a warm cache.
		PromptCacheKey: req.SessionID,
	}

	if req.ToolChoice.Mode != "" {
		chatReq.ToolChoice = buildCompatToolChoice(req.ToolChoice)
	}
	if req.Temperature > 0 {
		v := float64(req.Temperature)
		chatReq.Temperature = &v
	}
	if req.TopP > 0 {
		v := float64(req.TopP)
		chatReq.TopP = &v
	}
	if req.MaxOutputTokens > 0 {
		v := req.MaxOutputTokens
		chatReq.MaxTokens = &v
	}

	if req.Debug {
		runtimeoutput.Printf("=== DEBUG: %s Stream Request ===\nProvider: %s\nURL: %s/chat/completions\nModel: %s\nMessages: %d\nTools: %d\nVenice params: %v\n===================================\n", p.name, p.Name(), p.baseURL, model, len(messages), len(tools), veniceParams)
	}

	resp, err := p.makeChatRequest(ctx, chatReq)
	if err != nil {
		return nil, fmt.Errorf("%s API request failed: %w", p.name, err)
	}
	if resp.StatusCode != 200 {
		return nil, newOpenAICompatStatusErrorFromResponse(p.name, resp)
	}

	return newEventStreamWithCancelHook(ctx, func() { _ = resp.Body.Close() }, func(ctx context.Context, send eventSender) error {
		return readVeniceStream(p.name, resp.Body, send)
	}), nil
}

// veniceStreamState keeps the provider's inline reasoning and tool-call state
// across SSE deltas. Unlike the generic compat parser, Venice may put reasoning
// in both a dedicated field and split <think> tags in content.
type veniceStreamState struct {
	name           string
	send           eventSender
	toolState      *compatToolState
	lastUsage      *Usage
	lastEventType  string
	inlineThink    inlineThinkParser
	sawVisibleText bool
}

func readVeniceStream(name string, body io.ReadCloser, send eventSender) error {
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	state := veniceStreamState{name: name, send: send, toolState: newCompatToolState()}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			state.lastEventType = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		if err := state.processData(data); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s streaming error: %w", name, err)
	}
	return state.finish()
}

func (s *veniceStreamState) processData(data string) error {
	var chatResp oaiChatResponse
	if err := json.Unmarshal([]byte(data), &chatResp); err != nil {
		if s.lastEventType == "error" {
			return fmt.Errorf("%s API error: %s", s.name, strings.TrimSpace(data))
		}
		s.lastEventType = ""
		return nil
	}
	if s.lastEventType == "error" || chatResp.Error != nil {
		errMsg := "unknown error"
		if chatResp.Error != nil {
			errMsg = chatResp.Error.Message
		}
		return fmt.Errorf("%s API error: %s", s.name, errMsg)
	}
	if chatResp.Usage != nil {
		cached := chatResp.Usage.PromptTokensDetails.CachedTokens
		written := chatResp.Usage.PromptTokensDetails.CacheCreationInputTokens
		s.lastUsage = &Usage{
			// prompt_tokens includes cache reads and (Claude) cache writes; report them separately like
			// the native Anthropic provider does.
			InputTokens:            max(chatResp.Usage.PromptTokens-cached-written, 0),
			OutputTokens:           chatResp.Usage.CompletionTokens,
			CachedInputTokens:      cached,
			CacheWriteTokens:       written,
			ProviderRawInputTokens: chatResp.Usage.PromptTokens,
			ProviderTotalTokens:    chatResp.Usage.TotalTokens,
			ReasoningTokens:        chatResp.Usage.CompletionTokensDetails.ReasoningTokens,
		}
	}
	for _, choice := range chatResp.Choices {
		if choice.Delta != nil {
			if err := s.processDelta(choice.Delta); err != nil {
				return err
			}
		}
	}
	s.lastEventType = ""
	return nil
}

func (s *veniceStreamState) processDelta(delta *oaiMessage) error {
	reasoning := delta.Reasoning
	if reasoning == "" {
		reasoning = delta.ReasoningContent
	}
	if content, ok := delta.Content.(string); ok && content != "" {
		if shouldParseVeniceInlineThink(&s.inlineThink, content, s.sawVisibleText) {
			for _, part := range s.inlineThink.Process(content) {
				if part.Reasoning {
					if err := s.sendReasoning(part.Text); err != nil {
						return err
					}
				} else if err := s.sendText(part.Text, reasoning); err != nil {
					return err
				}
			}
		} else if err := s.sendText(content, reasoning); err != nil {
			return err
		}
	}
	if reasoning != "" {
		if err := s.sendReasoning(reasoning); err != nil {
			return err
		}
	}
	if len(delta.ToolCalls) > 0 {
		s.toolState.Add(delta.ToolCalls)
	}
	return nil
}

func (s *veniceStreamState) sendText(text, reasoning string) error {
	if isLeadingReasoningWhitespaceArtifact(text, reasoning, s.sawVisibleText) {
		return nil
	}
	if hasVisibleTextDelta(text) {
		s.sawVisibleText = true
	}
	return s.send.Send(Event{Type: EventTextDelta, Text: text})
}

func (s *veniceStreamState) sendReasoning(text string) error {
	return s.send.Send(Event{Type: EventReasoningDelta, Text: text, ReasoningKind: ReasoningKindRaw})
}

func (s *veniceStreamState) finish() error {
	for _, part := range s.inlineThink.Flush() {
		if part.Reasoning {
			if err := s.sendReasoning(part.Text); err != nil {
				return err
			}
		} else if part.Text != "" {
			// Pending content at EOF was never paired with a reasoning delta.
			if err := s.sendText(part.Text, ""); err != nil {
				return err
			}
		}
	}
	if err := s.toolState.Validate(); err != nil {
		return err
	}
	for _, call := range s.toolState.Calls() {
		if err := s.send.Send(Event{Type: EventToolCall, Tool: &call}); err != nil {
			return err
		}
	}
	if s.lastUsage != nil {
		if err := s.send.Send(Event{Type: EventUsage, Use: s.lastUsage}); err != nil {
			return err
		}
	}
	return s.send.Send(Event{Type: EventDone})
}

func shouldParseVeniceInlineThink(parser *inlineThinkParser, content string, sawVisibleText bool) bool {
	if parser != nil && (parser.inThink || parser.pending != "") {
		return true
	}
	if sawVisibleText {
		return false
	}
	trimmed := strings.TrimLeft(content, " \t\r\n")
	if trimmed == "" {
		return false
	}
	return strings.HasPrefix(trimmed, "<think>") || strings.HasPrefix("<think>", trimmed)
}

func buildVeniceModelAndParams(model string, search bool) (string, map[string]interface{}) {
	baseModel, suffixParams := parseVeniceModelSuffix(model)
	params := make(map[string]interface{}, len(suffixParams)+1)
	for k, v := range suffixParams {
		params[k] = v
	}
	if search {
		if _, hasX := params["enable_x_search"]; !hasX {
			if _, hasWeb := params["enable_web_search"]; !hasWeb {
				params["enable_web_search"] = "on"
			}
		}
	}
	if len(params) == 0 {
		return baseModel, nil
	}
	return baseModel, params
}

func parseVeniceModelSuffix(model string) (string, map[string]interface{}) {
	parts := strings.SplitN(model, ":", 2)
	if len(parts) < 2 {
		return model, nil
	}
	params := map[string]interface{}{}
	for _, raw := range strings.Split(parts[1], "&") {
		if raw == "" {
			continue
		}
		kv := strings.SplitN(raw, "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		params[kv[0]] = parseVeniceParamValue(kv[1])
	}
	if len(params) == 0 {
		return model, nil
	}
	return parts[0], params
}

func parseVeniceParamValue(v string) interface{} {
	switch strings.ToLower(v) {
	case "true":
		return true
	case "false":
		return false
	}
	if i, err := strconv.Atoi(v); err == nil {
		return i
	}
	return v
}

// isVeniceClaudeModel reports whether a Venice model id is an Anthropic Claude model, which only caches up to
// explicit cache_control breakpoints.
func isVeniceClaudeModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "claude-")
}

// veniceToolCallPlaceholder is the text block given to tool-call-only assistant turns for Claude. Venice rejects
// empty text blocks and offers no other place for a breakpoint on such a turn; the placeholder must be sent on
// EVERY such turn so the cached prefix stays byte-identical from request to request.
const veniceToolCallPlaceholder = " "

// applyVeniceClaudeCacheBreakpoint places a rolling cache_control breakpoint on the latest assistant message.
//
// Venice adds Claude breakpoints itself only on the system prompt and the second-to-last USER message
// (docs.venice.ai/guides/features/prompt-caching). An agent loop has a single user message followed by
// assistant/tool pairs, so without this only the system prompt is ever cached. Venice ignores cache_control on
// tool messages, tool calls and assistant messages, and rejects top-level cache_control; a marker on an
// assistant content part works. Measured on claude-haiku-5-5 (4 tool turns): cached tokens per request went
// from a constant ~4.3k (system prompt only) to 4.4k, 8.4k, 12.5k (the whole conversation except the newest
// tool result).
func applyVeniceClaudeCacheBreakpoint(messages []oaiMessage) {
	last := -1
	for i := range messages {
		if messages[i].Role != "assistant" {
			continue
		}
		switch c := messages[i].Content.(type) {
		case string:
			if c == "" {
				c = veniceToolCallPlaceholder
			}
			messages[i].Content = []oaiContentPart{{Type: "text", Text: c}}
		case []oaiContentPart:
			if len(c) == 0 {
				messages[i].Content = []oaiContentPart{{Type: "text", Text: veniceToolCallPlaceholder}}
			}
		case nil:
			messages[i].Content = []oaiContentPart{{Type: "text", Text: veniceToolCallPlaceholder}}
		default:
			continue
		}
		last = i
	}
	if last < 0 {
		return
	}
	parts := messages[last].Content.([]oaiContentPart)
	parts[len(parts)-1].CacheControl = &oaiCacheControl{Type: "ephemeral"}
}
