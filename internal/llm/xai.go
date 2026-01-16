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
	"strings"
)

const (
	xaiBaseURL = "https://api.x.ai/v1"
)

// XAIProvider implements Provider for the xAI (Grok) API.
// Uses OpenAI-compatible chat completions for tool calling,
// and the Responses API for native web/X search.
type XAIProvider struct {
	apiKey string
	model  string
}

// NewXAIProvider creates a new xAI provider.
func NewXAIProvider(apiKey, model string) *XAIProvider {
	if model == "" {
		model = "grok-4-1-fast"
	}
	return &XAIProvider{
		apiKey: apiKey,
		model:  model,
	}
}

func (p *XAIProvider) Name() string {
	return fmt.Sprintf("xAI (%s)", p.model)
}

func (p *XAIProvider) Credential() string {
	return "api_key"
}

func (p *XAIProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch: true,
		NativeWebFetch:  false,
		ToolCalls:       true,
	}
}

func (p *XAIProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	if req.Search {
		return p.streamWithSearch(ctx, req)
	}
	return p.streamStandard(ctx, req)
}

// streamStandard uses the OpenAI-compatible chat completions endpoint.
func (p *XAIProvider) streamStandard(ctx context.Context, req Request) (Stream, error) {
	messages := buildCompatMessages(req.Messages)
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}

	tools, err := buildCompatTools(req.Tools)
	if err != nil {
		return nil, err
	}

	chatReq := oaiChatRequest{
		Model:         chooseModel(req.Model, p.model),
		Messages:      messages,
		Tools:         tools,
		Stream:        true,
		StreamOptions: &oaiStreamOptions{IncludeUsage: true},
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
		fmt.Fprintf(os.Stderr, "=== DEBUG: xAI Stream Request ===\n")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "URL: %s/chat/completions\n", xaiBaseURL)
		fmt.Fprintf(os.Stderr, "Messages: %d\n", len(messages))
		fmt.Fprintf(os.Stderr, "Tools: %d\n", len(tools))
		fmt.Fprintln(os.Stderr, "=================================")
	}

	resp, err := p.makeChatRequest(ctx, chatReq)
	if err != nil {
		return nil, fmt.Errorf("xAI API request failed: %w", err)
	}

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("xAI API error (status %d): %s", resp.StatusCode, string(body))
	}

	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		toolState := newCompatToolState()
		tagStripper := newXAITagStripper()
		var lastUsage *Usage
		var lastEventType string

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
				return fmt.Errorf("xAI API error: %s", errMsg)
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
						// Use buffered tag stripper to handle tags split across chunks
						if stripped := tagStripper.Add(content); stripped != "" {
							events <- Event{Type: EventTextDelta, Text: stripped}
						}
					}
					if len(choice.Delta.ToolCalls) > 0 {
						toolState.Add(choice.Delta.ToolCalls)
					}
				}
				if choice.Message != nil {
					if content, ok := choice.Message.Content.(string); ok && content != "" {
						if stripped := tagStripper.Add(content); stripped != "" {
							events <- Event{Type: EventTextDelta, Text: stripped}
						}
					}
				}
			}

			lastEventType = ""
		}

		// Flush any remaining buffered content
		if remaining := tagStripper.Flush(); remaining != "" {
			events <- Event{Type: EventTextDelta, Text: remaining}
		}

		if err := scanner.Err(); err != nil {
			return fmt.Errorf("xAI streaming error: %w", err)
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

// streamWithSearch uses the xAI Responses API for native web and X search.
func (p *XAIProvider) streamWithSearch(ctx context.Context, req Request) (Stream, error) {
	input := buildXAIInput(req.Messages)
	if len(input) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}

	// Build search tools - include both web_search and x_search
	tools := []xaiResponsesTool{
		{Type: "web_search"},
		{Type: "x_search"},
	}

	responsesReq := xaiResponsesRequest{
		Model:  chooseModel(req.Model, p.model),
		Input:  input,
		Tools:  tools,
		Stream: true,
	}

	if req.Debug {
		fmt.Fprintf(os.Stderr, "=== DEBUG: xAI Responses API Request ===\n")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "URL: %s/responses\n", xaiBaseURL)
		fmt.Fprintf(os.Stderr, "Input items: %d\n", len(input))
		fmt.Fprintf(os.Stderr, "Tools: web_search, x_search\n")
		fmt.Fprintln(os.Stderr, "========================================")
	}

	resp, err := p.makeResponsesRequest(ctx, responsesReq)
	if err != nil {
		return nil, fmt.Errorf("xAI Responses API request failed: %w", err)
	}

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("xAI Responses API error (status %d): %s", resp.StatusCode, string(body))
	}

	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		var lastUsage *Usage
		var searchStarted bool

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}

			var event xaiResponsesEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}

			switch event.Type {
			case "response.output_item.added":
				// Check if this is a search tool being invoked
				if event.Item != nil && (event.Item.Type == "web_search_call" || event.Item.Type == "x_search_call") {
					if !searchStarted {
						searchStarted = true
						events <- Event{Type: EventPhase, Text: "Searching"}
						events <- Event{Type: EventToolExecStart, ToolName: event.Item.Type}
					}
				}

			case "response.output_item.done":
				// Search tool completed
				if event.Item != nil && (event.Item.Type == "web_search_call" || event.Item.Type == "x_search_call") {
					events <- Event{Type: EventToolExecEnd, ToolName: event.Item.Type}
				}

			case "response.output_text.delta":
				if event.Delta != "" {
					events <- Event{Type: EventTextDelta, Text: event.Delta}
				}

			case "response.completed":
				if event.Response != nil && event.Response.Usage != nil {
					lastUsage = &Usage{
						InputTokens:  event.Response.Usage.InputTokens,
						OutputTokens: event.Response.Usage.OutputTokens,
					}
				}

			case "error":
				return fmt.Errorf("xAI Responses API error: %s", event.Error)
			}
		}

		if err := scanner.Err(); err != nil {
			return fmt.Errorf("xAI Responses streaming error: %w", err)
		}

		if lastUsage != nil {
			events <- Event{Type: EventUsage, Use: lastUsage}
		}
		events <- Event{Type: EventDone}
		return nil
	}), nil
}

func (p *XAIProvider) makeChatRequest(ctx context.Context, req oaiChatRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return p.makeHTTPRequest(ctx, xaiBaseURL+"/chat/completions", body)
}

func (p *XAIProvider) makeResponsesRequest(ctx context.Context, req xaiResponsesRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return p.makeHTTPRequest(ctx, xaiBaseURL+"/responses", body)
}

func (p *XAIProvider) makeHTTPRequest(ctx context.Context, url string, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	return defaultHTTPClient.Do(httpReq)
}

// ListModels returns available models from the xAI API.
func (p *XAIProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", xaiBaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := defaultHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("xAI API error (status %d): %s", resp.StatusCode, string(body))
	}

	var modelsResp oaiModelsResponse
	if err := json.Unmarshal(body, &modelsResp); err != nil {
		return nil, fmt.Errorf("failed to parse models response: %w", err)
	}

	models := make([]ModelInfo, len(modelsResp.Data))
	for i, m := range modelsResp.Data {
		models[i] = ModelInfo{
			ID:      m.ID,
			Created: m.Created,
			OwnedBy: m.OwnedBy,
		}
	}
	return models, nil
}

// xAI Responses API types

type xaiResponsesRequest struct {
	Model       string              `json:"model"`
	Input       []xaiResponsesInput `json:"input"`
	Tools       []xaiResponsesTool  `json:"tools,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
	Temperature *float64            `json:"temperature,omitempty"`
}

type xaiResponsesInput struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type xaiResponsesTool struct {
	Type string `json:"type"` // "web_search" or "x_search"
}

type xaiResponsesEvent struct {
	Type     string                  `json:"type"`
	Delta    string                  `json:"delta,omitempty"`
	Item     *xaiResponsesItem       `json:"item,omitempty"`
	Response *xaiResponsesCompletion `json:"response,omitempty"`
	Error    string                  `json:"error,omitempty"`
}

type xaiResponsesItem struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

type xaiResponsesCompletion struct {
	Usage *xaiResponsesUsage `json:"usage,omitempty"`
}

type xaiResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func buildXAIInput(messages []Message) []xaiResponsesInput {
	var result []xaiResponsesInput
	for _, msg := range messages {
		var text string
		for _, part := range msg.Parts {
			if part.Type == PartText {
				text += part.Text
			}
		}
		if text == "" {
			continue
		}
		role := string(msg.Role)
		if role == "tool" {
			role = "user" // Responses API doesn't have tool role, use user
		}
		result = append(result, xaiResponsesInput{
			Role:    role,
			Content: text,
		})
	}
	return result
}

// stripXAITags removes leaked internal xAI tags from model output.
// This is a workaround for a known Grok model bug where internal
// function call markers leak into visible response text.
func stripXAITags(s string) string {
	// Known leaked tags from xAI models
	tags := []string{
		"<has_function_call>",
		"</has_function_call>",
		"<xai:function_call>",
		"</xai:function_call>",
	}
	for _, tag := range tags {
		s = strings.ReplaceAll(s, tag, "")
	}
	return s
}

// xaiTagStripper buffers streaming content to strip xAI internal tags
// that may be split across multiple stream chunks.
type xaiTagStripper struct {
	buffer string
	tags   []string
}

func newXAITagStripper() *xaiTagStripper {
	return &xaiTagStripper{
		tags: []string{
			"<has_function_call>",
			"</has_function_call>",
			"<xai:function_call>",
			"</xai:function_call>",
		},
	}
}

// Add adds content to the buffer and returns any content safe to emit.
// Content is held back if it might be the start of a tag.
func (s *xaiTagStripper) Add(content string) string {
	s.buffer += content

	// Strip any complete tags
	for _, tag := range s.tags {
		s.buffer = strings.ReplaceAll(s.buffer, tag, "")
	}

	// Find the longest potential tag prefix at the end of the buffer
	holdBack := 0
	for _, tag := range s.tags {
		for i := 1; i < len(tag) && i <= len(s.buffer); i++ {
			suffix := s.buffer[len(s.buffer)-i:]
			if strings.HasPrefix(tag, suffix) {
				if i > holdBack {
					holdBack = i
				}
			}
		}
	}

	// Emit everything except potential tag prefix
	if holdBack >= len(s.buffer) {
		return ""
	}
	emit := s.buffer[:len(s.buffer)-holdBack]
	s.buffer = s.buffer[len(s.buffer)-holdBack:]
	return emit
}

// Flush returns any remaining buffered content.
func (s *xaiTagStripper) Flush() string {
	result := s.buffer
	s.buffer = ""
	// Final strip in case buffer contains partial tag that never completed
	for _, tag := range s.tags {
		result = strings.ReplaceAll(result, tag, "")
	}
	return result
}
