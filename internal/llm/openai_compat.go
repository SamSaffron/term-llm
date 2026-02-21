package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// httpClientTimeout is the default timeout for HTTP requests
const httpClientTimeout = 10 * time.Minute

// defaultHTTPClient is a shared HTTP client with reasonable timeouts
var defaultHTTPClient = &http.Client{
	Timeout: httpClientTimeout,
}

// OpenAICompatProvider implements Provider for OpenAI-compatible APIs
// Used by Ollama, LM Studio, and other compatible servers.
type OpenAICompatProvider struct {
	baseURL string // Base URL - /chat/completions is appended
	chatURL string // Full chat URL - used as-is (optional, overrides baseURL)
	apiKey  string // Optional, most servers ignore it
	model   string
	name    string // Display name: "Ollama", "LM Studio", etc.
	headers map[string]string
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
	return fmt.Sprintf("%s (%s)", p.name, p.model)
}

func (p *OpenAICompatProvider) Credential() string {
	if p.apiKey == "" {
		return "free"
	}
	return "api_key"
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
	Model             string            `json:"model"`
	Messages          []oaiMessage      `json:"messages"`
	Tools             []oaiTool         `json:"tools,omitempty"`
	ToolChoice        interface{}       `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	MaxTokens         *int              `json:"max_tokens,omitempty"`
	Stream            bool              `json:"stream,omitempty"`
	StreamOptions     *oaiStreamOptions `json:"stream_options,omitempty"`
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
	Index    int    `json:"index,omitempty"`
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
}

type oaiAPIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
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
	Name    string           `json:"name,omitempty"`
	Pricing *oaiModelPricing `json:"pricing,omitempty"`
}

type oaiModelPricing struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
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
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
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
			InputLimit:  InputLimitForModel(m.ID),
		}
		// Parse OpenRouter-style pricing (per-token prices as strings)
		if m.Pricing != nil {
			if v, err := strconv.ParseFloat(m.Pricing.Prompt, 64); err == nil {
				info.InputPrice = v * 1_000_000 // Convert to per-million tokens
			}
			if v, err := strconv.ParseFloat(m.Pricing.Completion, 64); err == nil {
				info.OutputPrice = v * 1_000_000
			}
		}
		models[i] = info
	}

	return models, nil
}

func (p *OpenAICompatProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	// Build messages and tools synchronously
	messages := buildCompatMessages(req.Messages)
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}

	tools, err := buildCompatTools(req.Tools)
	if err != nil {
		return nil, err
	}

	chatReq := oaiChatRequest{
		Model:    chooseModel(req.Model, p.model),
		Messages: messages,
		Tools:    tools,
		Stream:   true,
	}

	if req.ToolChoice.Mode != "" {
		chatReq.ToolChoice = buildCompatToolChoice(req.ToolChoice)
	}
	if req.ParallelToolCalls {
		chatReq.ParallelToolCalls = boolPtr(true)
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
		fmt.Fprintf(os.Stderr, "=== DEBUG: %s Stream Request ===\n", p.name)
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "URL: %s/chat/completions\n", p.baseURL)
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
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("%s API error (status %d): %s", p.name, resp.StatusCode, string(body))
	}

	// Only create async stream for successful HTTP responses
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		toolState := newCompatToolState()
		var lastUsage *Usage
		var lastEventType string
		var reasoningBuilder strings.Builder

		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				lastEventType = strings.TrimPrefix(line, "event: ")
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}

			var chatResp oaiChatResponse
			if err := json.Unmarshal([]byte(data), &chatResp); err != nil {
				continue
			}

			if lastEventType == "error" || chatResp.Error != nil {
				errMsg := "unknown error"
				if chatResp.Error != nil {
					errMsg = chatResp.Error.Message
				}
				return fmt.Errorf("%s API error: %s", p.name, errMsg)
			}

			if chatResp.Usage != nil {
				lastUsage = &Usage{
					InputTokens:       chatResp.Usage.PromptTokens,
					OutputTokens:      chatResp.Usage.CompletionTokens,
					CachedInputTokens: chatResp.Usage.PromptTokensDetails.CachedTokens,
				}
			}

			for _, choice := range chatResp.Choices {
				if choice.Delta != nil {
					if content, ok := choice.Delta.Content.(string); ok && content != "" {
						events <- Event{Type: EventTextDelta, Text: content}
					}
					// Capture reasoning from thinking models (OpenRouter streaming uses "reasoning" field)
					if choice.Delta.Reasoning != "" {
						reasoningBuilder.WriteString(choice.Delta.Reasoning)
						events <- Event{Type: EventReasoningDelta, Text: choice.Delta.Reasoning}
					}
					if len(choice.Delta.ToolCalls) > 0 {
						toolState.Add(choice.Delta.ToolCalls)
					}
				}
			}

			lastEventType = ""
		}

		if err := scanner.Err(); err != nil {
			return fmt.Errorf("%s streaming error: %w", p.name, err)
		}

		for _, call := range toolState.Calls() {
			events <- Event{Type: EventToolCall, Tool: &call}
		}
		if lastUsage != nil {
			events <- Event{Type: EventUsage, Use: lastUsage}
		}
		events <- Event{Type: EventDone}
		return nil
	}), nil
}

func buildCompatMessages(messages []Message) []oaiMessage {
	messages = sanitizeToolHistory(messages)

	var result []oaiMessage
	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem, RoleUser, RoleAssistant:
			text, toolCalls, reasoning := splitParts(msg.Parts)
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
					if part.Type == PartImage && part.ImageData != nil {
						dataURL := fmt.Sprintf("data:%s;base64,%s", part.ImageData.MediaType, part.ImageData.Base64)
						imageParts = append(imageParts, oaiContentPart{Type: "image_url", ImageURL: &oaiImageURL{URL: dataURL, Detail: "auto"}})
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

				var richParts []oaiContentPart
				hasImage := false
				for _, contentPart := range toolResultContentParts(part.ToolResult) {
					switch contentPart.Type {
					case ToolContentPartText:
						if contentPart.Text != "" {
							richParts = append(richParts, oaiContentPart{Type: "text", Text: contentPart.Text})
						}
					case ToolContentPartImageData:
						mimeType, base64Data, ok := toolResultImageData(contentPart)
						if !ok {
							continue
						}
						hasImage = true
						dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64Data)
						richParts = append(richParts, oaiContentPart{Type: "image_url", ImageURL: &oaiImageURL{URL: dataURL, Detail: "auto"}})
					}
				}
				if hasImage && len(richParts) > 0 {
					result = append(result, oaiMessage{
						Role:    "user",
						Content: richParts,
					})
				}
			}
		}
	}
	return result
}

// splitParts extracts text, tool calls, and reasoning content from message parts.
// Returns (text, toolCalls, reasoningContent).
func splitParts(parts []Part) (string, []oaiToolCall, string) {
	var textParts []string
	var toolCalls []oaiToolCall
	var reasoning string
	for _, part := range parts {
		switch part.Type {
		case PartText:
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
		schema, err := json.Marshal(spec.Schema)
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
	byIndex map[int]*toolCallState
	order   []int
}

type toolCallState struct {
	id   string
	name string
	args strings.Builder
}

func newCompatToolState() *compatToolState {
	return &compatToolState{byIndex: make(map[int]*toolCallState)}
}

func (s *compatToolState) Add(calls []oaiToolCall) {
	for _, call := range calls {
		idx := call.Index
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

func (s *compatToolState) Calls() []ToolCall {
	if len(s.order) == 0 {
		return nil
	}
	sort.Ints(s.order)
	calls := make([]ToolCall, 0, len(s.order))
	for _, idx := range s.order {
		state := s.byIndex[idx]
		if state == nil {
			continue
		}
		calls = append(calls, ToolCall{
			ID:        state.id,
			Name:      state.name,
			Arguments: json.RawMessage(state.args.String()),
		})
	}
	return calls
}

func boolPtr(v bool) *bool {
	return &v
}
