package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

// newStreamingHTTPClient creates an HTTP client with transport-level timeouts.
// It clones the default transport to retain HTTP/2 and standard connection-pool
// behavior, while allowing more concurrent provider connections to remain idle.
func newStreamingHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 2 * time.Minute
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxIdleConnsPerHost = 100
	return &http.Client{Transport: transport}
}

// defaultHTTPClient is a shared HTTP client with transport-level timeouts.
//
// http.Client.Timeout is intentionally NOT set: it applies to the entire
// request lifetime including reading the streaming response body, and would
// abort legitimate long-running streams.
//
// Instead we use Transport-level timeouts that only cover connection
// establishment and the initial response headers, so hung connections fail
// fast while active streams are never killed.
var defaultHTTPClient = newStreamingHTTPClient()

// OpenAICompatProvider implements Provider for OpenAI-compatible APIs
// Used by Ollama, LM Studio, and other compatible servers.
type OpenAICompatProvider struct {
	baseURL           string // Base URL - /chat/completions is appended
	chatURL           string // Full chat URL - used as-is (optional, overrides baseURL)
	apiKey            string // Optional, most servers ignore it
	model             string
	effort            string // reasoning effort: "low", "medium", "high", "xhigh", or ""
	name              string // Display name: "Ollama", "LM Studio", etc.
	headers           map[string]string
	noStreamOptions   bool                         // If true, don't send stream_options (for servers that reject it)
	vllmThinking      bool                         // If true, send vLLM thinking controls instead of reasoning_effort
	vllmThinkingParam string                       // Optional chat_template_kwargs key override ("thinking" for DeepSeek, "enable_thinking" for Qwen)
	parseReasoning    *bool                        // Optional parse_reasoning request flag for compatible reasoning parsers
	includeReasoning  *bool                        // Optional include_reasoning request flag for compatible reasoning parsers
	thinkingParam     string                       // Optional chat_template_kwargs key set to true when reasoning effort is requested
	modelConfigs      []config.ProviderModelConfig // Optional per-model aliases/metadata from config
}

func NewOpenAICompatProvider(baseURL, apiKey, model, name string) *OpenAICompatProvider {
	return NewOpenAICompatProviderWithHeaders(baseURL, apiKey, model, name, nil)
}

func NewOpenAICompatProviderWithHeaders(baseURL, apiKey, model, name string, headers map[string]string) *OpenAICompatProvider {
	return NewOpenAICompatProviderFull(baseURL, "", apiKey, model, name, headers)
}

// NewOpenAICompatProviderFull creates a provider with full control over URLs.
// If chatURL is provided, it's used directly for chat completions (no path appending).
// If only baseURL is provided, /chat/completions is appended.
// baseURL is normalized to strip /chat/completions if accidentally included.
func NewOpenAICompatProviderFull(baseURL, chatURL, apiKey, model, name string, headers map[string]string) *OpenAICompatProvider {
	// Normalize baseURL - strip trailing slash and common endpoint paths
	// This allows users to paste the full URL from documentation
	if baseURL != "" {
		baseURL = strings.TrimSuffix(baseURL, "/")
		baseURL = strings.TrimSuffix(baseURL, "/chat/completions")
		baseURL = strings.TrimSuffix(baseURL, "/") // In case URL was .../v1/chat/completions
		// Local LLM servers (Ollama, LM Studio, vLLM, etc.) bind to 127.0.0.1
		// (IPv4) by default. If the user passes "localhost", Go may resolve it to
		// ::1 (IPv6) on dual-stack systems, causing connection refused even when
		// the server is running. Normalise to the explicit IPv4 loopback address.
		baseURL = strings.Replace(baseURL, "://localhost:", "://127.0.0.1:", 1)
	}
	// Normalize chatURL - just strip trailing slash
	if chatURL != "" {
		chatURL = strings.TrimSuffix(chatURL, "/")
	}
	return &OpenAICompatProvider{
		baseURL: baseURL,
		chatURL: chatURL,
		apiKey:  apiKey,
		model:   model,
		name:    name,
		headers: headers,
	}
}

func (p *OpenAICompatProvider) Name() string {
	if p.effort != "" {
		return fmt.Sprintf("%s (%s, effort=%s)", p.name, p.model, p.effort)
	}
	return fmt.Sprintf("%s (%s)", p.name, p.model)
}

func (p *OpenAICompatProvider) Credential() string {
	if p.apiKey == "" {
		return "free"
	}
	return "api_key"
}

func (p *OpenAICompatProvider) SetReasoningParser(parseReasoning, includeReasoning *bool, thinkingParam string) {
	p.parseReasoning = parseReasoning
	p.includeReasoning = includeReasoning
	p.thinkingParam = strings.TrimSpace(thinkingParam)
}

func (p *OpenAICompatProvider) SetModelConfigs(modelConfigs []config.ProviderModelConfig) {
	p.modelConfigs = cloneProviderModelConfigs(modelConfigs)
}

func cloneProviderModelConfigs(modelConfigs []config.ProviderModelConfig) []config.ProviderModelConfig {
	if len(modelConfigs) == 0 {
		return nil
	}
	out := append([]config.ProviderModelConfig(nil), modelConfigs...)
	for i := range out {
		out[i].ReasoningEfforts = append([]string(nil), out[i].ReasoningEfforts...)
	}
	return out
}

func (p *OpenAICompatProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    false,
		NativeWebFetch:     false,
		ToolCalls:          true,
		SupportsToolChoice: true, // OpenAI API supports tool_choice
	}
}

// OpenAI-compatible request/response structures
// Tool choice can be string ("none"/"auto") or object.
type oaiChatRequest struct {
	Model               string                 `json:"model"`
	Messages            []oaiMessage           `json:"messages"`
	Tools               []oaiTool              `json:"tools,omitempty"`
	ToolChoice          interface{}            `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool                  `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort     string                 `json:"reasoning_effort,omitempty"`
	Temperature         *float64               `json:"temperature,omitempty"`
	TopP                *float64               `json:"top_p,omitempty"`
	MaxTokens           *int                   `json:"max_tokens,omitempty"`
	Stream              bool                   `json:"stream,omitempty"`
	StreamOptions       *oaiStreamOptions      `json:"stream_options,omitempty"`
	ChatTemplateKwargs  map[string]interface{} `json:"chat_template_kwargs,omitempty"`
	ThinkingTokenBudget *int                   `json:"thinking_token_budget,omitempty"`
	ParseReasoning      *bool                  `json:"parse_reasoning,omitempty"`
	IncludeReasoning    *bool                  `json:"include_reasoning,omitempty"`
	VeniceParameters    map[string]interface{} `json:"venice_parameters,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiMessage struct {
	Role             string        `json:"role"`
	Content          interface{}   `json:"content,omitempty"` // string or []oaiContentPart for multimodal
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"` // For sending to API (thinking models)
	Reasoning        string        `json:"reasoning,omitempty"`         // For receiving from API delta (thinking models)
}

type oaiContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type oaiTool struct {
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaiToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

type oaiChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Model   string       `json:"model"`
	Choices []oaiChoice  `json:"choices"`
	Usage   *oaiUsage    `json:"usage,omitempty"`
	Error   *oaiAPIError `json:"error,omitempty"`
}

type oaiChoice struct {
	Index        int         `json:"index"`
	Message      *oaiMessage `json:"message,omitempty"`
	Delta        *oaiMessage `json:"delta,omitempty"`
	FinishReason string      `json:"finish_reason"`
}

type oaiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type oaiAPIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func newOpenAICompatStatusErrorFromResponse(provider string, resp *http.Response) *HTTPStatusError {
	body := providerhttp.ReadBodyAndClose(resp, 0)
	errMsg := string(body)
	var errResp oaiChatResponse
	if json.Unmarshal(body, &errResp) == nil && errResp.Error != nil && errResp.Error.Message != "" {
		errMsg = errResp.Error.Message
	}
	return newHTTPStatusErrorWithDisplayBody(provider, resp, body, errMsg)
}

// Model listing structures
type oaiModelsResponse struct {
	Data []oaiModel `json:"data"`
}

type oaiModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// OpenRouter-specific fields
	Name          string               `json:"name,omitempty"`
	Pricing       *oaiModelPricing     `json:"pricing,omitempty"`
	ContextLength int                  `json:"context_length,omitempty"`
	TopProvider   *oaiModelTopProvider `json:"top_provider,omitempty"`
}

type oaiModelTopProvider struct {
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
}

type oaiModelPricing struct {
	Prompt     oaiFlexibleFloat `json:"prompt"`
	Completion oaiFlexibleFloat `json:"completion"`
}

type oaiFlexibleFloat float64

func (f *oaiFlexibleFloat) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*f = 0
		return nil
	}
	var n float64
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*f = 0
			return nil
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		n = v
	} else {
		if err := json.Unmarshal(data, &n); err != nil {
			return err
		}
	}
	*f = oaiFlexibleFloat(n)
	return nil
}

func (f oaiFlexibleFloat) Float64() float64 {
	return float64(f)
}

func normalizeCompatModelPrice(price float64) float64 {
	if price > 0 && price < 0.01 {
		return price * 1_000_000
	}
	return price
}

func (p *OpenAICompatProvider) makeRequest(ctx context.Context, method, endpoint string, body []byte) (*http.Response, error) {
	url := p.baseURL + endpoint

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for key, value := range p.headers {
		if value == "" {
			continue
		}
		httpReq.Header.Set(key, value)
	}

	return defaultHTTPClient.Do(httpReq)
}

func (p *OpenAICompatProvider) makeChatRequest(ctx context.Context, req oaiChatRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// Use chatURL directly if set, otherwise use baseURL + endpoint
	if p.chatURL != "" {
		return p.makeRequestToURL(ctx, "POST", p.chatURL, body)
	}
	return p.makeRequest(ctx, "POST", "/chat/completions", body)
}

func (p *OpenAICompatProvider) makeRequestToURL(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for key, value := range p.headers {
		if value == "" {
			continue
		}
		httpReq.Header.Set(key, value)
	}

	return defaultHTTPClient.Do(httpReq)
}

// ListModels returns available models from the server.
func (p *OpenAICompatProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	resp, err := p.makeRequest(ctx, "GET", "/models", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, newHTTPStatusError("", resp, body)
	}

	var modelsResp oaiModelsResponse
	if err := json.Unmarshal(body, &modelsResp); err != nil {
		return nil, fmt.Errorf("failed to parse models response: %w", err)
	}

	models := make([]ModelInfo, len(modelsResp.Data))
	for i, m := range modelsResp.Data {
		info := ModelInfo{
			ID:          m.ID,
			DisplayName: m.Name,
			Created:     m.Created,
			OwnedBy:     m.OwnedBy,
		}
		if m.ContextLength > 0 {
			info.InputLimit = m.ContextLength
		} else {
			info.InputLimit = InputLimitForProviderModel(strings.ToLower(p.name), m.ID)
		}
		// Parse pricing. OpenRouter reports per-token prices as small decimals;
		// some OpenAI-compatible providers report per-million prices directly.
		if m.Pricing != nil {
			info.InputPrice = normalizeCompatModelPrice(m.Pricing.Prompt.Float64())
			info.OutputPrice = normalizeCompatModelPrice(m.Pricing.Completion.Float64())
		}
		models[i] = info
	}

	return models, nil
}

type openAICompatResolvedModel struct {
	Model            string
	Effort           string
	ParseReasoning   *bool
	IncludeReasoning *bool
	ThinkingParam    string
	MaxOutputTokens  int
}

func (p *OpenAICompatProvider) resolveConfiguredModel(model string) openAICompatResolvedModel {
	model = strings.TrimSpace(model)
	resolved := openAICompatResolvedModel{
		Model:            model,
		ParseReasoning:   p.parseReasoning,
		IncludeReasoning: p.includeReasoning,
		ThinkingParam:    p.thinkingParam,
	}

	for _, entry := range p.modelConfigs {
		for _, name := range compatModelNames(entry) {
			if model == name {
				resolved.Model = strings.TrimSpace(entry.ID)
				if resolved.Model == "" {
					resolved.Model = name
				}
				resolved.apply(entry)
				return resolved
			}
		}
	}
	for _, entry := range p.modelConfigs {
		for _, effort := range entry.ReasoningEfforts {
			effort = strings.TrimSpace(effort)
			if effort == "" {
				continue
			}
			for _, name := range compatModelNames(entry) {
				if model == name+"-"+effort {
					resolved.Model = strings.TrimSpace(entry.ID)
					if resolved.Model == "" {
						resolved.Model = name
					}
					resolved.Effort = effort
					resolved.apply(entry)
					return resolved
				}
			}
		}
	}

	parsedModel, parsedEffort := ParseModelEffort(model)
	if parsedEffort != "" && p.hasConfiguredModelName(parsedModel) {
		return resolved
	}
	resolved.Model = parsedModel
	resolved.Effort = parsedEffort
	return resolved
}

func (p *OpenAICompatProvider) hasConfiguredModelName(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, entry := range p.modelConfigs {
		for _, name := range compatModelNames(entry) {
			if model == name {
				return true
			}
		}
	}
	return false
}

func (r *openAICompatResolvedModel) apply(entry config.ProviderModelConfig) {
	if entry.ParseReasoning != nil {
		r.ParseReasoning = entry.ParseReasoning
	}
	if entry.IncludeReasoning != nil {
		r.IncludeReasoning = entry.IncludeReasoning
	}
	if thinkingParam := strings.TrimSpace(entry.ThinkingParam); thinkingParam != "" {
		r.ThinkingParam = thinkingParam
	}
	if entry.MaxOutputTokens > 0 {
		r.MaxOutputTokens = entry.MaxOutputTokens
	}
}

func clampCompatOutputTokens(requested int, model string, configuredLimit int) int {
	if requested <= 0 {
		return requested
	}
	if configuredLimit > 0 && requested > configuredLimit {
		return configuredLimit
	}
	return ClampOutputTokens(requested, model)
}

func compatModelNames(entry config.ProviderModelConfig) []string {
	seen := make(map[string]bool, 2)
	var names []string
	appendName := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	appendName(entry.Alias)
	appendName(entry.ID)
	return names
}

func (p *OpenAICompatProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	// Effort precedence: explicit request effort wins over model suffix, which wins over provider-level effort.
	configuredModel := chooseModel(req.Model, p.model)
	resolvedModel := p.resolveConfiguredModel(configuredModel)
	model := resolvedModel.Model
	effort := p.effort
	if resolvedModel.Effort != "" {
		effort = resolvedModel.Effort
	}
	if v := strings.TrimSpace(req.ReasoningEffort); v != "" {
		effort = v
	}
	req.MaxOutputTokens = clampCompatOutputTokens(req.MaxOutputTokens, model, resolvedModel.MaxOutputTokens)
	// Build messages and tools synchronously
	messages := buildCompatMessages(req.Messages)
	if p.vllmThinking {
		messages = convertCompatMessagesForVLLM(messages)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}

	tools, err := buildCompatTools(req.Tools)
	if err != nil {
		return nil, err
	}

	chatReq := oaiChatRequest{
		Model:           model,
		Messages:        messages,
		Tools:           tools,
		Stream:          true,
		ReasoningEffort: effort,
	}
	if p.vllmThinking {
		kwargs, budget, vllmReasoningEffort := vLLMThinkingSettings(model, effort, p.vllmThinkingParam)
		chatReq.ReasoningEffort = vllmReasoningEffort
		chatReq.ChatTemplateKwargs = kwargs
		if budget > 0 {
			chatReq.ThinkingTokenBudget = &budget
		}
	}
	if resolvedModel.ParseReasoning != nil {
		chatReq.ParseReasoning = resolvedModel.ParseReasoning
	}
	if resolvedModel.IncludeReasoning != nil {
		chatReq.IncludeReasoning = resolvedModel.IncludeReasoning
	}
	if effort != "" && resolvedModel.ThinkingParam != "" {
		if chatReq.ChatTemplateKwargs == nil {
			chatReq.ChatTemplateKwargs = map[string]interface{}{}
		}
		chatReq.ChatTemplateKwargs[resolvedModel.ThinkingParam] = true
	}
	if !p.noStreamOptions {
		chatReq.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}

	if req.ToolChoice.Mode != "" {
		chatReq.ToolChoice = buildCompatToolChoice(req.ToolChoice)
	}
	if len(tools) > 0 {
		chatReq.ParallelToolCalls = boolPtr(req.ParallelToolCalls)
	}
	if req.TemperatureSet || req.Temperature != 0 {
		v := float64(req.Temperature)
		chatReq.Temperature = &v
	}
	if req.TopPSet || req.TopP != 0 {
		v := float64(req.TopP)
		chatReq.TopP = &v
	}
	if req.MaxOutputTokens > 0 {
		v := req.MaxOutputTokens
		chatReq.MaxTokens = &v
	}

	if req.Debug {
		fmt.Fprintf(os.Stderr, "=== DEBUG: %s Stream Request ===\n", p.name)
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "URL: %s/chat/completions\n", p.baseURL)
		fmt.Fprintf(os.Stderr, "Model: %s\n", model)
		if effort != "" {
			fmt.Fprintf(os.Stderr, "ReasoningEffort: %s\n", effort)
		}
		fmt.Fprintf(os.Stderr, "Messages: %d\n", len(messages))
		fmt.Fprintf(os.Stderr, "Tools: %d\n", len(tools))
		fmt.Fprintln(os.Stderr, "===================================")
	}

	// Make HTTP request synchronously - this allows retry wrapper to catch errors like 429
	resp, err := p.makeChatRequest(ctx, chatReq)
	if err != nil {
		return nil, fmt.Errorf("%s API request failed: %w", p.name, err)
	}

	// Check for error responses synchronously so retry logic can handle them
	if resp.StatusCode != 200 {
		return nil, newOpenAICompatStatusErrorFromResponse(p.name, resp)
	}

	// Only create async stream for successful HTTP responses
	return newEventStreamWithCancelHook(ctx, func() { _ = resp.Body.Close() }, func(ctx context.Context, send eventSender) error {
		defer resp.Body.Close()

		decoder := newSSEDecoder(resp.Body, sseDecoderOptions{RequireDone: false, Transport: p.name + " SSE"})

		toolState := newCompatToolState()
		var lastUsage *Usage
		var reasoningBuilder strings.Builder
		sawVisibleText := false
		sawToolCallsFinish := false

		for {
			eventType, data, err := decoder.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("%s streaming error: %w", p.name, err)
			}

			var chatResp oaiChatResponse
			if err := json.Unmarshal(data, &chatResp); err != nil {
				return fmt.Errorf("%s streaming error: invalid JSON chunk: %w", p.name, err)
			}

			if eventType == "error" || chatResp.Error != nil {
				errMsg := "unknown error"
				if chatResp.Error != nil {
					errMsg = chatResp.Error.Message
				}
				return fmt.Errorf("%s API error: %s", p.name, errMsg)
			}

			if chatResp.Usage != nil {
				cached := chatResp.Usage.PromptTokensDetails.CachedTokens
				lastUsage = &Usage{
					// OpenAI prompt_tokens includes cached; subtract to get non-cached portion.
					// CachedInputTokens + InputTokens = total context size.
					InputTokens:            chatResp.Usage.PromptTokens - cached,
					OutputTokens:           chatResp.Usage.CompletionTokens,
					CachedInputTokens:      cached,
					ProviderRawInputTokens: chatResp.Usage.PromptTokens,
					ProviderTotalTokens:    chatResp.Usage.TotalTokens,
					ReasoningTokens:        chatResp.Usage.CompletionTokensDetails.ReasoningTokens,
				}
			}

			for _, choice := range chatResp.Choices {
				if choice.FinishReason == "tool_calls" {
					sawToolCallsFinish = true
				}
				if choice.Delta != nil {
					// Capture reasoning from thinking models. Different OpenAI-compatible
					// backends use either "reasoning" (OpenRouter) or
					// "reasoning_content" (DeepSeek) in streaming deltas.
					reasoningDelta := choice.Delta.Reasoning
					if reasoningDelta == "" {
						reasoningDelta = choice.Delta.ReasoningContent
					}
					if content, ok := choice.Delta.Content.(string); ok && content != "" {
						// Some OpenAI-compatible reasoning models emit a pure-whitespace
						// assistant content prefix (commonly "\n\n") in the same delta as
						// reasoning_content. That prefix is an artifact of the hidden-thinking
						// channel and creates visible blank gaps before tool status rows. Do not
						// globally trim leading whitespace: only suppress this known reasoning
						// artifact, and preserve whitespace once visible text has started.
						isReasoningWhitespaceArtifact := isLeadingReasoningWhitespaceArtifact(content, reasoningDelta, sawVisibleText)
						if !isReasoningWhitespaceArtifact {
							if hasVisibleTextDelta(content) {
								sawVisibleText = true
							}
							if err := send.Send(Event{Type: EventTextDelta, Text: content}); err != nil {
								return err
							}
						}
					}
					if reasoningDelta != "" {
						reasoningBuilder.WriteString(reasoningDelta)
						if err := send.Send(Event{Type: EventReasoningDelta, Text: reasoningDelta, ReasoningKind: ReasoningKindRaw}); err != nil {
							return err
						}
					}
					if len(choice.Delta.ToolCalls) > 0 {
						toolState.Add(choice.Delta.ToolCalls)
					}
				}
			}
		}

		sawDone := decoder.DoneSeen()
		if !sawDone && !sawToolCallsFinish {
			return &StreamIncompleteError{Transport: p.name + " SSE", Terminal: "[DONE]"}
		}
		if err := toolState.Validate(); err != nil {
			if !sawDone {
				return &StreamIncompleteError{Transport: p.name + " SSE", Terminal: "[DONE]", Err: err}
			}
			return err
		}
		for _, call := range toolState.Calls() {
			if err := send.Send(Event{Type: EventToolCall, Tool: &call}); err != nil {
				return err
			}
		}
		if lastUsage != nil {
			if err := send.Send(Event{Type: EventUsage, Use: lastUsage}); err != nil {
				return err
			}
		}
		if !sawDone {
			return &StreamIncompleteError{Transport: p.name + " SSE", Terminal: "[DONE]"}
		}
		if err := send.Send(Event{Type: EventDone}); err != nil {
			return err
		}
		return nil
	}), nil
}

func buildCompatMessages(messages []Message) []oaiMessage {
	messages = sanitizeToolHistory(messages)

	var result []oaiMessage
	var pendingDev string
	var pendingToolImages []oaiContentPart
	flushToolImages := func() {
		if len(pendingToolImages) == 0 {
			return
		}
		result = append(result, oaiMessage{Role: "user", Content: pendingToolImages})
		pendingToolImages = nil
	}
	for _, msg := range messages {
		if msg.Role != RoleTool {
			// Keep every result for one assistant tool-call turn contiguous. vLLM's
			// DeepSeek renderer merges and orders only contiguous tool messages.
			flushToolImages()
		}
		switch msg.Role {
		case RoleDeveloper:
			// OpenAI-compatible backends have no native developer role.
			// Buffer the text and prepend it into the next user turn
			// wrapped in <developer> tags.
			text, _, _ := splitParts(msg.Parts)
			if text != "" {
				if pendingDev != "" {
					pendingDev += "\n\n"
				}
				pendingDev += text
			}
		case RoleSystem, RoleUser, RoleAssistant:
			text, toolCalls, reasoning := splitParts(msg.Parts)
			if msg.Role == RoleUser && pendingDev != "" {
				text = fmt.Sprintf("<developer>\n%s\n</developer>\n\n", pendingDev) + text
				pendingDev = ""
			}
			if msg.Role == RoleAssistant && len(toolCalls) > 0 {
				result = append(result, oaiMessage{
					Role:             "assistant",
					Content:          text,
					ToolCalls:        toolCalls,
					ReasoningContent: reasoning, // For thinking models (OpenRouter)
				})
				continue
			}
			// Check for user messages with images — build multimodal content.
			if msg.Role == RoleUser {
				var imageParts []oaiContentPart
				for _, part := range msg.Parts {
					if part.Type == PartImage && part.ImageData != nil && strings.TrimSpace(part.ImageData.Base64) != "" {
						dataURL := fmt.Sprintf("data:%s;base64,%s", part.ImageData.MediaType, part.ImageData.Base64)
						imageParts = append(imageParts, oaiContentPart{Type: "image_url", ImageURL: &oaiImageURL{URL: dataURL, Detail: imageDetail(part.ImageData.Detail)}})
						if part.ImagePath != "" {
							imageParts = append(imageParts, oaiContentPart{Type: "text", Text: "[image saved at: " + part.ImagePath + "]"})
						}
					}
				}
				if len(imageParts) > 0 {
					var contentParts []oaiContentPart
					if text != "" {
						contentParts = append(contentParts, oaiContentPart{Type: "text", Text: text})
					}
					contentParts = append(contentParts, imageParts...)
					result = append(result, oaiMessage{Role: "user", Content: contentParts})
					continue
				}
			}
			if text == "" && reasoning == "" {
				continue
			}
			role := string(msg.Role)
			result = append(result, oaiMessage{Role: role, Content: text, ReasoningContent: reasoning})
		case RoleTool:
			for _, part := range msg.Parts {
				if part.Type != PartToolResult || part.ToolResult == nil {
					continue
				}
				textContent := toolResultTextContent(part.ToolResult)

				// Add tool result with text content
				result = append(result, oaiMessage{
					Role:       "tool",
					Content:    textContent,
					ToolCallID: part.ToolResult.ID,
				})

				// Inject a synthetic user message with image parts only.
				// Text is already in the tool result above — don't duplicate it.
				var imageParts []oaiContentPart
				for _, contentPart := range toolResultContentParts(part.ToolResult) {
					if contentPart.Type != ToolContentPartImageData {
						continue
					}
					mimeType, base64Data, ok := toolResultImageData(contentPart)
					if !ok {
						continue
					}
					dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64Data)
					imageParts = append(imageParts, oaiContentPart{Type: "image_url", ImageURL: &oaiImageURL{URL: dataURL, Detail: imageDetailWithDefault(contentPart.ImageData.Detail, "auto")}})
				}
				if len(imageParts) > 0 {
					pendingToolImages = append(pendingToolImages, imageParts...)
				}
			}
		}
	}
	// Flush trailing tool images before a synthetic trailing developer turn so
	// the images stay adjacent to the tool results that produced them.
	flushToolImages()
	// Trailing developer message with no following user turn.
	if pendingDev != "" {
		result = append(result, oaiMessage{
			Role:    "user",
			Content: fmt.Sprintf("<developer>\n%s\n</developer>", pendingDev),
		})
	}
	return result
}

func convertCompatMessagesForVLLM(messages []oaiMessage) []oaiMessage {
	for i := range messages {
		if messages[i].Role == "assistant" && messages[i].ReasoningContent != "" {
			// vLLM renamed reasoning_content to reasoning. Use the current field
			// for assistant-message replay so Qwen reasoning is rendered by vLLM's
			// chat template and can participate in prefix caching.
			messages[i].Reasoning = messages[i].ReasoningContent
			messages[i].ReasoningContent = ""
		}
	}
	return messages
}

// splitParts extracts text, tool calls, and reasoning content from message parts.
// Returns (text, toolCalls, reasoningContent).
func splitParts(parts []Part) (string, []oaiToolCall, string) {
	var textParts []string
	var toolCalls []oaiToolCall
	var reasoning string
	for _, part := range parts {
		switch part.Type {
		case PartText, PartFile:
			if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
			// Capture reasoning_content from thinking models
			if part.ReasoningContent != "" {
				reasoning = part.ReasoningContent
			}
		case PartToolCall:
			if part.ToolCall == nil {
				continue
			}
			toolCalls = append(toolCalls, oaiToolCall{
				ID:   part.ToolCall.ID,
				Type: "function",
				Function: struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				}{
					Name:      part.ToolCall.Name,
					Arguments: string(part.ToolCall.Arguments),
				},
			})
		}
	}
	return strings.Join(textParts, ""), toolCalls, reasoning
}

func buildCompatTools(specs []ToolSpec) ([]oaiTool, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	tools := make([]oaiTool, 0, len(specs))
	for _, spec := range specs {
		schema, err := cachedToolSchemaJSON(spec.Schema)
		if err != nil {
			return nil, fmt.Errorf("marshal tool schema %s: %w", spec.Name, err)
		}
		tools = append(tools, oaiTool{
			Type: "function",
			Function: oaiFunction{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters:  schema,
			},
		})
	}
	return tools, nil
}

func buildCompatToolChoice(choice ToolChoice) interface{} {
	switch choice.Mode {
	case ToolChoiceNone:
		return "none"
	case ToolChoiceRequired:
		return "required"
	case ToolChoiceAuto:
		return "auto"
	case ToolChoiceName:
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]string{"name": choice.Name},
		}
	default:
		return nil
	}
}

type compatToolState struct {
	byIndex            map[int]*toolCallState
	order              []int
	implicitByPosition map[int]int
	nextImplicitIndex  int
	hasImplicitIndex   bool
}

type toolCallState struct {
	id   string
	name string
	args strings.Builder
}

func newCompatToolState() *compatToolState {
	return &compatToolState{
		byIndex:            make(map[int]*toolCallState),
		implicitByPosition: make(map[int]int),
		nextImplicitIndex:  -1,
	}
}

func (s *compatToolState) Add(calls []oaiToolCall) {
	for pos, call := range calls {
		idx := 0
		if call.Index != nil {
			idx = *call.Index
			if mappedIdx, ok := s.implicitByPosition[pos]; ok && mappedIdx < 0 {
				idx = mappedIdx
			}
		} else {
			s.hasImplicitIndex = true
			var ok bool
			idx, ok = s.implicitByPosition[pos]
			if !ok {
				idx = s.nextImplicitIndex
				s.nextImplicitIndex--
				s.implicitByPosition[pos] = idx
			}
		}
		state, ok := s.byIndex[idx]
		if !ok {
			state = &toolCallState{}
			s.byIndex[idx] = state
			s.order = append(s.order, idx)
		}
		if call.ID != "" {
			state.id = call.ID
		}
		if call.Function.Name != "" {
			state.name = call.Function.Name
		}
		if call.Function.Arguments != "" {
			state.args.WriteString(call.Function.Arguments)
		}
	}
}

func (s *compatToolState) Validate() error {
	if len(s.order) == 0 {
		return nil
	}
	for _, idx := range s.order {
		state := s.byIndex[idx]
		if state == nil {
			continue
		}
		if strings.TrimSpace(state.id) == "" {
			return fmt.Errorf("OpenAI-compatible stream missing tool call id for tool call %d", idx)
		}
		if strings.TrimSpace(state.name) == "" {
			return fmt.Errorf("OpenAI-compatible stream missing tool name for tool call %d", idx)
		}
		args := strings.TrimSpace(state.args.String())
		if args != "" && !json.Valid([]byte(args)) {
			return fmt.Errorf("OpenAI-compatible stream invalid arguments for tool call %d", idx)
		}
	}
	return nil
}

func (s *compatToolState) Calls() []ToolCall {
	if len(s.order) == 0 {
		return nil
	}
	order := append([]int(nil), s.order...)
	if !s.hasImplicitIndex {
		sort.Ints(order)
	}
	calls := make([]ToolCall, 0, len(order))
	for _, idx := range order {
		state := s.byIndex[idx]
		if state == nil {
			continue
		}
		argsString := strings.TrimSpace(state.args.String())
		args := json.RawMessage("{}")
		if argsString != "" {
			args = json.RawMessage(argsString)
		}
		calls = append(calls, ToolCall{
			ID:        state.id,
			Name:      state.name,
			Arguments: args,
		})
	}
	return calls
}

func boolPtr(v bool) *bool {
	return &v
}
