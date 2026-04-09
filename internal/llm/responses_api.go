package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxResponsesAPIErrorBodyBytes = 64 * 1024

var truncatedResponsesAPIErrorBodySuffix = []byte("\n... response body truncated")

// ResponsesClient makes raw HTTP calls to Open Responses-compliant endpoints.
// See https://www.openresponses.org/specification
type ResponsesClient struct {
	BaseURL            string            // Full URL for responses endpoint (e.g., "https://api.openai.com/v1/responses")
	GetAuthHeader      func() string     // Dynamic auth (allows token refresh)
	ExtraHeaders       map[string]string // Provider-specific headers
	HTTPClient         *http.Client      // HTTP client to use
	LastResponseID     string            // Track for conversation continuity (server state)
	DisableServerState bool              // Set to true to disable previous_response_id (e.g., for Copilot)
	// HandleError, if set, is called for non-200 responses before default handling.
	// Return a non-nil error to short-circuit; return nil to fall through to defaults.
	HandleError func(statusCode int, body []byte, headers http.Header) error
	// OnAuthRetry, if set, is called when a 401/403 is received.
	// The current request context is passed so that refresh operations
	// use a live context rather than a potentially canceled one.
	// If it returns nil (success), the request is retried with fresh credentials.
	// If it returns an error, that error is returned to the caller.
	OnAuthRetry func(ctx context.Context) error
}

// ResponsesRequest follows the Open Responses spec
type ResponsesRequest struct {
	Model              string               `json:"model"`
	Instructions       string               `json:"instructions,omitempty"` // System instructions (alternative to developer-role input items)
	Input              []ResponsesInputItem `json:"input"`
	Tools              []any                `json:"tools,omitempty"` // Can contain ResponsesTool or ResponsesWebSearchTool
	ToolChoice         any                  `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens    int                  `json:"max_output_tokens,omitempty"`
	Temperature        *float64             `json:"temperature,omitempty"`
	TopP               *float64             `json:"top_p,omitempty"`
	Reasoning          *ResponsesReasoning  `json:"reasoning,omitempty"`
	Include            []string             `json:"include,omitempty"`
	PromptCacheKey     string               `json:"prompt_cache_key,omitempty"`
	Store              *bool                `json:"store,omitempty"`
	Stream             bool                 `json:"stream"`
	PreviousResponseID string               `json:"previous_response_id,omitempty"`
	SessionID          string               `json:"-"`
}

// ResponsesWebSearchTool represents the web search tool for OpenAI
type ResponsesWebSearchTool struct {
	Type string `json:"type"` // "web_search_preview"
}

// ResponsesInputItem represents an input item in the Open Responses format
type ResponsesInputItem struct {
	Type    string      `json:"type"`
	Role    string      `json:"role,omitempty"`
	Content interface{} `json:"content,omitempty"` // string or []ResponsesContentPart
	// For reasoning type
	ID               string                     `json:"id,omitempty"`
	EncryptedContent string                     `json:"encrypted_content,omitempty"`
	Summary          *responsesReasoningSummary `json:"summary,omitempty"`
	// For function_call type
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// For function_call_output type
	Output string `json:"output,omitempty"`
}

// ResponsesContentPart represents a content part (text or image)
type ResponsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"` // Plain URL string for Responses API (not object)
}

// ResponsesTool represents a tool definition in Open Responses format
type ResponsesTool struct {
	Type        string                 `json:"type"`
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters"`
	Strict      bool                   `json:"strict,omitempty"`
}

// ResponsesReasoning configures reasoning effort for models that support it
type ResponsesReasoning struct {
	Effort  string `json:"effort,omitempty"`  // "low", "medium", "high", "xhigh"
	Summary string `json:"summary,omitempty"` // "auto"
}

// responsesAPIResponse is the response structure from the API
type responsesAPIResponse struct {
	ID     string                `json:"id"`
	Object string                `json:"object"`
	Output []responsesOutputItem `json:"output"`
	Usage  *responsesUsage       `json:"usage,omitempty"`
	Error  *responsesError       `json:"error,omitempty"`
}

type responsesOutputItem struct {
	Type    string                   `json:"type"` // "message" or "function_call"
	Content []responsesOutputContent `json:"content,omitempty"`
	// For reasoning
	EncryptedContent string                          `json:"encrypted_content,omitempty"`
	Summary          []responsesReasoningSummaryPart `json:"summary,omitempty"`
	// For function_call
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type responsesReasoningSummaryPart struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text,omitempty"`
}

type responsesReasoningSummary []responsesReasoningSummaryPart

type responsesOutputContent struct {
	Type    string `json:"type"` // "output_text" or "refusal"
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type responsesError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// SSE event types from the streaming response
type responsesSSEEvent struct {
	Type     string          `json:"type"`
	Response json.RawMessage `json:"response,omitempty"`
	Delta    struct {
		Text string `json:"text,omitempty"`
	} `json:"delta,omitempty"`
	// For function call events
	Item        *responsesOutputItem `json:"item,omitempty"`
	ItemID      string               `json:"item_id,omitempty"`
	OutputIndex int                  `json:"output_index,omitempty"`
}

// BuildResponsesInputWithInstructions converts []Message to Open Responses input
// format, extracting system messages as a separate instructions string instead of
// including them as developer-role input items. This is used by providers that send
// system content via the "instructions" request field (e.g., ChatGPT).
func BuildResponsesInputWithInstructions(messages []Message) (instructions string, input []ResponsesInputItem) {
	messages = sanitizeToolHistory(messages)
	var systemParts []string
	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			if text := collectTextParts(msg.Parts); text != "" {
				systemParts = append(systemParts, text)
			}
		default:
			input = append(input, buildResponsesInputForRole(msg)...)
		}
	}
	return strings.Join(systemParts, "\n\n"), input
}

// BuildResponsesInput converts []Message to Open Responses input format
func BuildResponsesInput(messages []Message) []ResponsesInputItem {
	messages = sanitizeToolHistory(messages)
	var inputItems []ResponsesInputItem
	for _, msg := range messages {
		if msg.Role == RoleSystem || msg.Role == RoleDeveloper {
			inputItems = append(inputItems, buildResponsesMessageItems("developer", msg.Parts)...)
		} else {
			inputItems = append(inputItems, buildResponsesInputForRole(msg)...)
		}
	}
	return inputItems
}

// buildResponsesInputForRole converts a single non-system message to input items.
func buildResponsesInputForRole(msg Message) []ResponsesInputItem {
	switch msg.Role {
	case RoleUser:
		return buildResponsesMessageItems("user", msg.Parts)
	case RoleDeveloper:
		return buildResponsesMessageItems("developer", msg.Parts)
	case RoleAssistant:
		return buildResponsesAssistantItems(msg.Parts)
	case RoleTool:
		var items []ResponsesInputItem
		for _, part := range msg.Parts {
			if part.Type != PartToolResult || part.ToolResult == nil {
				continue
			}
			callID := strings.TrimSpace(part.ToolResult.ID)
			if callID == "" {
				continue
			}
			items = append(items, ResponsesInputItem{
				Type:   "function_call_output",
				CallID: callID,
				Output: toolResultTextContent(part.ToolResult),
			})
			if richParts, hasImage := toolResultResponsesImageParts(part.ToolResult); hasImage {
				items = append(items, ResponsesInputItem{
					Type:    "message",
					Role:    "user",
					Content: richParts,
				})
			}
		}
		return items
	default:
		return nil
	}
}

func buildResponsesMessageItems(role string, parts []Part) []ResponsesInputItem {
	var items []ResponsesInputItem
	var textBuf strings.Builder

	flushText := func() {
		if textBuf.Len() == 0 {
			return
		}
		items = append(items, ResponsesInputItem{
			Type:    "message",
			Role:    role,
			Content: textBuf.String(),
		})
		textBuf.Reset()
	}

	for _, part := range parts {
		switch part.Type {
		case PartText:
			if part.Text != "" {
				textBuf.WriteString(part.Text)
			}
		case PartImage:
			if part.ImageData != nil {
				flushText()
				dataURL := fmt.Sprintf("data:%s;base64,%s", part.ImageData.MediaType, part.ImageData.Base64)
				imageParts := []ResponsesContentPart{{Type: "input_image", ImageURL: dataURL}}
				if part.ImagePath != "" {
					imageParts = append(imageParts, ResponsesContentPart{Type: "input_text", Text: "[image saved at: " + part.ImagePath + "]"})
				}
				items = append(items, ResponsesInputItem{
					Type:    "message",
					Role:    role,
					Content: imageParts,
				})
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
			items = append(items, ResponsesInputItem{
				Type:      "function_call",
				CallID:    callID,
				Name:      part.ToolCall.Name,
				Arguments: args,
			})
		}
	}

	flushText()
	return items
}

func buildResponsesAssistantItems(parts []Part) []ResponsesInputItem {
	var items []ResponsesInputItem
	var textBuf strings.Builder

	flushText := func() {
		if textBuf.Len() == 0 {
			return
		}
		items = append(items, ResponsesInputItem{
			Type:    "message",
			Role:    "assistant",
			Content: textBuf.String(),
		})
		textBuf.Reset()
	}

	for _, part := range parts {
		switch part.Type {
		case PartText:
			if hasResponsesReasoningReplay(part) {
				flushText()
				items = append(items, buildResponsesReasoningItem(part))
			}
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
			items = append(items, ResponsesInputItem{
				Type:      "function_call",
				CallID:    callID,
				Name:      part.ToolCall.Name,
				Arguments: args,
			})
		}
	}

	flushText()
	return items
}

func hasResponsesReasoningReplay(part Part) bool {
	return strings.TrimSpace(part.ReasoningItemID) != "" || strings.TrimSpace(part.ReasoningEncryptedContent) != ""
}

func buildResponsesReasoningItem(part Part) ResponsesInputItem {
	summary := responsesReasoningSummary{}
	item := ResponsesInputItem{
		Type:             "reasoning",
		ID:               strings.TrimSpace(part.ReasoningItemID),
		EncryptedContent: strings.TrimSpace(part.ReasoningEncryptedContent),
		Summary:          &summary,
	}
	if strings.TrimSpace(part.ReasoningContent) != "" {
		summary = append(summary, responsesReasoningSummaryPart{
			Type: "summary_text",
			Text: strings.TrimSpace(part.ReasoningContent),
		})
		item.Summary = &summary
	}
	return item
}

// BuildResponsesTools converts []ToolSpec to Open Responses format with schema normalization
func BuildResponsesTools(specs []ToolSpec) []any {
	if len(specs) == 0 {
		return nil
	}
	tools := make([]any, 0, len(specs))
	for _, spec := range specs {
		var params map[string]interface{}
		strict := true
		if spec.NoStrict {
			params = normalizeSchemaForOpenAI(spec.Schema)
			strict = false
		} else {
			params = normalizeSchemaForOpenAIStrict(spec.Schema)
		}
		tools = append(tools, ResponsesTool{
			Type:        "function",
			Name:        spec.Name,
			Description: spec.Description,
			Parameters:  params,
			Strict:      strict,
		})
	}
	return tools
}

// BuildResponsesToolChoice converts ToolChoice to Open Responses format
func BuildResponsesToolChoice(choice ToolChoice) interface{} {
	switch choice.Mode {
	case ToolChoiceNone:
		return "none"
	case ToolChoiceRequired:
		return "required"
	case ToolChoiceAuto:
		return "auto"
	case ToolChoiceName:
		return map[string]interface{}{
			"type": "function",
			"name": choice.Name,
		}
	default:
		return nil
	}
}

func readResponsesAPIErrorBody(resp *http.Response) []byte {
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponsesAPIErrorBodyBytes+1))
	if len(body) <= maxResponsesAPIErrorBodyBytes {
		return body
	}

	truncated := make([]byte, 0, maxResponsesAPIErrorBodyBytes+len(truncatedResponsesAPIErrorBodySuffix))
	truncated = append(truncated, body[:maxResponsesAPIErrorBodyBytes]...)
	truncated = append(truncated, truncatedResponsesAPIErrorBodySuffix...)
	return truncated
}

// Stream makes a streaming request to the Responses API and returns events via a Stream
func (c *ResponsesClient) Stream(ctx context.Context, req ResponsesRequest, debugRaw bool) (Stream, error) {
	fullInput := req.Input

	// Use server state: send previous_response_id if we have one (unless disabled)
	if !c.DisableServerState && c.LastResponseID != "" {
		req.PreviousResponseID = c.LastResponseID
		// When continuing a conversation, only send the new input items for this turn.
		req.Input = filterToNewInput(req.Input)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	if debugRaw {
		var prettyBody bytes.Buffer
		json.Indent(&prettyBody, body, "", "  ")
		DebugRawSection(debugRaw, "Responses API Request", prettyBody.String())
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.GetAuthHeader != nil {
		httpReq.Header.Set("Authorization", c.GetAuthHeader())
	}
	if req.SessionID != "" {
		httpReq.Header.Set("session_id", req.SessionID)
	}
	for key, value := range c.ExtraHeaders {
		httpReq.Header.Set(key, value)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = defaultHTTPClient
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Responses API request failed: %w", err)
	}

	// Check for error responses synchronously so retry logic can handle them
	if resp.StatusCode != http.StatusOK {
		respBody := readResponsesAPIErrorBody(resp)

		if debugRaw {
			var debugInfo strings.Builder
			debugInfo.WriteString(fmt.Sprintf("Status: %d %s\n", resp.StatusCode, resp.Status))
			debugInfo.WriteString("Headers:\n")
			for key, values := range resp.Header {
				for _, value := range values {
					debugInfo.WriteString(fmt.Sprintf("  %s: %s\n", key, value))
				}
			}
			debugInfo.WriteString("Body:\n")
			var prettyBody bytes.Buffer
			if json.Indent(&prettyBody, respBody, "", "  ") == nil {
				debugInfo.WriteString(prettyBody.String())
			} else {
				debugInfo.WriteString(string(respBody))
			}
			DebugRawSection(debugRaw, "Responses API Error Response", debugInfo.String())
		}

		// Provider-specific error handling (e.g., ChatGPT rate limits)
		if c.HandleError != nil {
			if err := c.HandleError(resp.StatusCode, respBody, resp.Header); err != nil {
				return nil, err
			}
		}

		// Check for previous_response_id not found error
		if resp.StatusCode == http.StatusNotFound && c.LastResponseID != "" {
			// Clear state and retry with full history
			c.LastResponseID = ""
			req.PreviousResponseID = ""
			req.Input = fullInput
			// Re-marshal without previous_response_id
			body, err = json.Marshal(req)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal retry request: %w", err)
			}
			httpReq, err = http.NewRequestWithContext(ctx, "POST", c.BaseURL, bytes.NewReader(body))
			if err != nil {
				return nil, fmt.Errorf("failed to create retry request: %w", err)
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Accept", "text/event-stream")
			if c.GetAuthHeader != nil {
				httpReq.Header.Set("Authorization", c.GetAuthHeader())
			}
			if req.SessionID != "" {
				httpReq.Header.Set("session_id", req.SessionID)
			}
			for key, value := range c.ExtraHeaders {
				httpReq.Header.Set(key, value)
			}
			resp, err = httpClient.Do(httpReq)
			if err != nil {
				return nil, fmt.Errorf("Responses API retry request failed: %w", err)
			}
			if resp.StatusCode != http.StatusOK {
				retryBody := readResponsesAPIErrorBody(resp)
				return nil, fmt.Errorf("Responses API error (status %d): %s", resp.StatusCode, string(retryBody))
			}
		} else if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			if c.OnAuthRetry != nil {
				if err := c.OnAuthRetry(ctx); err != nil {
					return nil, err
				}
				// Retry with refreshed credentials
				httpReq, err = http.NewRequestWithContext(ctx, "POST", c.BaseURL, bytes.NewReader(body))
				if err != nil {
					return nil, fmt.Errorf("failed to create auth retry request: %w", err)
				}
				httpReq.Header.Set("Content-Type", "application/json")
				httpReq.Header.Set("Accept", "text/event-stream")
				if c.GetAuthHeader != nil {
					httpReq.Header.Set("Authorization", c.GetAuthHeader())
				}
				if req.SessionID != "" {
					httpReq.Header.Set("session_id", req.SessionID)
				}
				for key, value := range c.ExtraHeaders {
					httpReq.Header.Set(key, value)
				}
				resp, err = httpClient.Do(httpReq)
				if err != nil {
					return nil, fmt.Errorf("Responses API auth retry request failed: %w", err)
				}
				if resp.StatusCode != http.StatusOK {
					retryBody := readResponsesAPIErrorBody(resp)
					return nil, fmt.Errorf("Responses API error after re-auth (status %d): %s", resp.StatusCode, string(retryBody))
				}
			} else {
				return nil, fmt.Errorf("Responses API authentication failed (status %d): token may be invalid or expired", resp.StatusCode)
			}
		} else {
			return nil, fmt.Errorf("Responses API error (status %d): %s", resp.StatusCode, string(respBody))
		}
	}

	// Capture client reference for the goroutine
	client := c

	// Create async stream for successful response
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		defer resp.Body.Close()

		reader := bufio.NewReader(resp.Body)

		// Tool call state for accumulating streaming function calls
		toolState := newResponsesToolState()
		reasoningState := newResponsesReasoningState()
		var lastUsage *Usage
		var lastEventType string
		var sawTextDelta bool // Track if any text deltas were emitted

		for {
			line, eof, err := readSSELine(reader)
			if err != nil {
				return fmt.Errorf("Responses API streaming error: %w", err)
			}
			if eof && line == "" {
				break
			}

			if strings.HasPrefix(line, "event: ") {
				lastEventType = strings.TrimPrefix(line, "event: ")
				if eof {
					break
				}
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				if eof {
					break
				}
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}

			if debugRaw {
				DebugRawSection(debugRaw, "Responses API SSE Line (event="+lastEventType+")", data)
			}

			// Handle different SSE event types based on event name
			switch lastEventType {
			case "response.output_text.delta":
				var deltaEvent struct {
					Delta string `json:"delta"`
				}
				if err := json.Unmarshal([]byte(data), &deltaEvent); err == nil && deltaEvent.Delta != "" {
					sawTextDelta = true
					events <- Event{Type: EventTextDelta, Text: deltaEvent.Delta}
				}

			case "response.output_item.added":
				var itemEvent struct {
					Item        responsesOutputItem `json:"item"`
					OutputIndex int                 `json:"output_index"`
				}
				if err := json.Unmarshal([]byte(data), &itemEvent); err == nil {
					if itemEvent.Item.Type == "function_call" {
						// Use output_index as tracking key (stable across events), Item.CallID as the actual call ID
						toolState.StartCall(itemEvent.OutputIndex, itemEvent.Item.CallID, itemEvent.Item.Name)
					} else if itemEvent.Item.Type == "reasoning" {
						reasoningState.Start(itemEvent.OutputIndex, itemEvent.Item.ID, itemEvent.Item.EncryptedContent, itemEvent.Item.Summary)
					}
				}

			case "response.function_call_arguments.delta":
				var argEvent struct {
					OutputIndex int    `json:"output_index"`
					Delta       string `json:"delta"`
				}
				if err := json.Unmarshal([]byte(data), &argEvent); err == nil {
					toolState.AppendArguments(argEvent.OutputIndex, argEvent.Delta)
				}

			case "response.output_item.done":
				var doneEvent struct {
					Item        responsesOutputItem `json:"item"`
					OutputIndex int                 `json:"output_index"`
				}
				if err := json.Unmarshal([]byte(data), &doneEvent); err == nil {
					if doneEvent.Item.Type == "function_call" {
						// Complete the tool call with final arguments using output_index
						toolState.FinishCall(doneEvent.OutputIndex, doneEvent.Item.CallID, doneEvent.Item.Name, doneEvent.Item.Arguments)
					} else if doneEvent.Item.Type == "reasoning" {
						reasoningState.Finish(doneEvent.OutputIndex, doneEvent.Item.ID, doneEvent.Item.EncryptedContent, doneEvent.Item.Summary)
						if part := reasoningState.Part(doneEvent.OutputIndex); part != nil {
							events <- Event{
								Type:                      EventReasoningDelta,
								Text:                      part.ReasoningContent,
								ReasoningItemID:           part.ReasoningItemID,
								ReasoningEncryptedContent: part.ReasoningEncryptedContent,
							}
						}
					} else if doneEvent.Item.Type == "message" {
						// Text content is normally streamed via response.output_text.delta events.
						// Fall back to emitting here if no deltas were seen (provider inconsistency).
						// Always emit refusals since those may not be streamed.
						for _, content := range doneEvent.Item.Content {
							if content.Type == "output_text" && content.Text != "" && !sawTextDelta {
								events <- Event{Type: EventTextDelta, Text: content.Text}
							} else if content.Type == "refusal" && content.Refusal != "" {
								events <- Event{Type: EventTextDelta, Text: content.Refusal}
							}
						}
					}
				}

			case "response.reasoning_summary_part.added":
				var partEvent struct {
					OutputIndex int `json:"output_index"`
				}
				if err := json.Unmarshal([]byte(data), &partEvent); err == nil {
					reasoningState.Ensure(partEvent.OutputIndex)
				}

			case "response.reasoning_summary_text.delta":
				var summaryDeltaEvent struct {
					OutputIndex int    `json:"output_index"`
					Delta       string `json:"delta"`
				}
				if err := json.Unmarshal([]byte(data), &summaryDeltaEvent); err == nil {
					reasoningState.AppendSummary(summaryDeltaEvent.OutputIndex, summaryDeltaEvent.Delta)
				}

			case "response.completed":
				var completedEvent struct {
					Response struct {
						ID    string          `json:"id"`
						Usage *responsesUsage `json:"usage,omitempty"`
					} `json:"response"`
				}
				if err := json.Unmarshal([]byte(data), &completedEvent); err == nil {
					// Store response ID for conversation continuity (unless disabled)
					if !client.DisableServerState && completedEvent.Response.ID != "" {
						client.LastResponseID = completedEvent.Response.ID
					}
					if completedEvent.Response.Usage != nil {
						cached := completedEvent.Response.Usage.InputTokensDetails.CachedTokens
						lastUsage = &Usage{
							// OpenAI Responses API input_tokens includes cached; subtract to normalise.
							// CachedInputTokens + InputTokens = total context size.
							InputTokens:       completedEvent.Response.Usage.InputTokens - cached,
							OutputTokens:      completedEvent.Response.Usage.OutputTokens,
							CachedInputTokens: cached,
						}
					}
				}

			case "response.failed", "error":
				var errorEvent struct {
					Error *responsesError `json:"error"`
				}
				if err := json.Unmarshal([]byte(data), &errorEvent); err == nil && errorEvent.Error != nil {
					return fmt.Errorf("Responses API error: %s", errorEvent.Error.Message)
				}
				return fmt.Errorf("Responses API error: unknown error")
			}

			lastEventType = ""
			if eof {
				break
			}
		}

		// Emit completed tool calls
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

// ResetConversation clears server state (called on /clear or new conversation)
func (c *ResponsesClient) ResetConversation() {
	c.LastResponseID = ""
}

// filterToNewInput returns only the new input items for a server-state continuation.
func filterToNewInput(input []ResponsesInputItem) []ResponsesInputItem {
	// Tool follow-up turns append function_call_output items (and sometimes
	// synthetic/user messages such as image parts) with no new user message.
	// When present at the end of the input, send only that trailing suffix.
	start := len(input)
	sawToolOutput := false
	for start > 0 {
		item := input[start-1]
		if item.Type == "function_call_output" {
			sawToolOutput = true
			start--
			continue
		}
		if item.Type == "message" && item.Role == "user" {
			start--
			continue
		}
		break
	}
	if sawToolOutput {
		return input[start:]
	}

	// Otherwise fall back to the latest user message and any following items.
	for i := len(input) - 1; i >= 0; i-- {
		if input[i].Role == "user" {
			return input[i:]
		}
	}
	return input
}

// responsesToolState tracks streaming tool calls from the Responses API
type responsesToolState struct {
	calls map[int]*responsesToolCallState // keyed by output_index (stable across events)
	order []int                           // order of output_index values
}

type responsesToolCallState struct {
	outputIndex int    // Output index - stable across added/delta/done events
	callID      string // Actual call ID (call_xxx) - used in tool results
	name        string
	args        strings.Builder
	finished    bool
}

func newResponsesToolState() *responsesToolState {
	return &responsesToolState{calls: make(map[int]*responsesToolCallState)}
}

// StartCall starts tracking a new tool call.
// outputIndex is the stable index across events, callID is the actual call ID (call_xxx).
func (s *responsesToolState) StartCall(outputIndex int, callID, name string) {
	if _, exists := s.calls[outputIndex]; exists {
		return
	}
	s.calls[outputIndex] = &responsesToolCallState{outputIndex: outputIndex, callID: callID, name: name}
	s.order = append(s.order, outputIndex)
}

func (s *responsesToolState) AppendArguments(outputIndex int, args string) {
	if state, ok := s.calls[outputIndex]; ok && !state.finished {
		state.args.WriteString(args)
	}
}

func (s *responsesToolState) FinishCall(outputIndex int, callID, name, finalArgs string) {
	state, ok := s.calls[outputIndex]
	if !ok {
		// Tool call not found by output_index - create it now (handles edge cases)
		s.calls[outputIndex] = &responsesToolCallState{outputIndex: outputIndex, callID: callID, name: name}
		s.order = append(s.order, outputIndex)
		state = s.calls[outputIndex]
	}
	if finalArgs != "" {
		// Use final args if provided (overwrite streamed)
		state.args.Reset()
		state.args.WriteString(finalArgs)
	}
	// Update callID if provided (in case it wasn't set initially)
	if callID != "" {
		state.callID = callID
	}
	// Update name if provided and current name is empty
	if name != "" && state.name == "" {
		state.name = name
	}
	state.finished = true
}

func (s *responsesToolState) Calls() []ToolCall {
	if len(s.order) == 0 {
		return nil
	}
	calls := make([]ToolCall, 0, len(s.order))
	for _, outputIndex := range s.order {
		state := s.calls[outputIndex]
		if state == nil {
			continue
		}
		args := state.args.String()
		if args == "" {
			args = "{}"
		}
		// Use callID for the tool call ID (this is what needs to be sent back in tool results)
		// callID should always be set from the done event if not from added
		id := state.callID
		if id == "" {
			// Fallback: generate a placeholder ID (shouldn't happen in practice)
			id = fmt.Sprintf("call_%d", outputIndex)
		}
		calls = append(calls, ToolCall{
			ID:        id,
			Name:      state.name,
			Arguments: json.RawMessage(args),
		})
	}
	return calls
}

type responsesReasoningState struct {
	items map[int]*responsesReasoningItemState
}

type responsesReasoningItemState struct {
	part Part
}

func newResponsesReasoningState() *responsesReasoningState {
	return &responsesReasoningState{
		items: make(map[int]*responsesReasoningItemState),
	}
}

func (s *responsesReasoningState) Ensure(outputIndex int) {
	if _, ok := s.items[outputIndex]; ok {
		return
	}
	s.items[outputIndex] = &responsesReasoningItemState{
		part: Part{Type: PartText},
	}
}

func (s *responsesReasoningState) Start(outputIndex int, itemID, encrypted string, summary []responsesReasoningSummaryPart) {
	s.Ensure(outputIndex)
	state := s.items[outputIndex]
	if itemID != "" {
		state.part.ReasoningItemID = itemID
	}
	if encrypted != "" {
		state.part.ReasoningEncryptedContent = encrypted
	}
	if text := extractReasoningSummaryText(summary); text != "" {
		state.part.ReasoningContent = text
	}
}

func (s *responsesReasoningState) AppendSummary(outputIndex int, delta string) {
	if delta == "" {
		return
	}
	s.Ensure(outputIndex)
	state := s.items[outputIndex]
	state.part.ReasoningContent += delta
}

func (s *responsesReasoningState) Finish(outputIndex int, itemID, encrypted string, summary []responsesReasoningSummaryPart) {
	s.Start(outputIndex, itemID, encrypted, summary)
}

func (s *responsesReasoningState) Part(outputIndex int) *Part {
	state, ok := s.items[outputIndex]
	if !ok {
		return nil
	}
	if state.part.ReasoningItemID == "" && state.part.ReasoningEncryptedContent == "" && state.part.ReasoningContent == "" {
		return nil
	}
	part := state.part
	return &part
}

func extractReasoningSummaryText(summary []responsesReasoningSummaryPart) string {
	if len(summary) == 0 {
		return ""
	}
	var text strings.Builder
	for _, part := range summary {
		if part.Type != "summary_text" || strings.TrimSpace(part.Text) == "" {
			continue
		}
		text.WriteString(part.Text)
	}
	return text.String()
}
