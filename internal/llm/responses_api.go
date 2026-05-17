package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const maxResponsesAPIErrorBodyBytes = 64 * 1024

var truncatedResponsesAPIErrorBodySuffix = []byte("\n... response body truncated")

const (
	responsesWebSocketMaxAttempts = 3
	responsesWebSocketMaxBackoff  = 2 * time.Second
)

var responsesWebSocketBaseBackoff = 250 * time.Millisecond

// ResponsesClient makes raw HTTP calls to Open Responses-compliant endpoints.
// See https://www.openresponses.org/specification
type ResponsesClient struct {
	BaseURL            string            // Full URL for responses endpoint (e.g., "https://api.openai.com/v1/responses")
	GetAuthHeader      func() string     // Dynamic auth (allows token refresh)
	ExtraHeaders       map[string]string // Provider-specific headers
	HTTPClient         *http.Client      // HTTP client to use
	LastResponseID     string            // Track for conversation continuity (server state)
	DisableServerState bool              // Set to true to disable previous_response_id (e.g., for Copilot)

	// Optional Responses-over-WebSocket transport, controlled by provider config.
	UseWebSocket bool
	// WebSocketServerState enables previous_response_id only for the WebSocket
	// transport while keeping HTTP/SSE full-history. This is used for ChatGPT,
	// whose WebSocket backend supports connection-local continuation but whose
	// HTTP endpoint may reject previous_response_id.
	WebSocketServerState    bool
	WebSocketURL            string
	WebSocketConnectTimeout time.Duration
	WebSocketWriteTimeout   time.Duration
	WebSocketIdleTimeout    time.Duration
	websocketDisabled       bool
	wsMu                    sync.Mutex
	wsConn                  *websocket.Conn
	wsLastRequest           *ResponsesRequest
	wsLastResponseItems     []ResponsesInputItem
	// HandleError, if set, is called for non-200 responses before default handling.
	// Return a non-nil error to short-circuit; return nil to fall through to defaults.
	HandleError func(statusCode int, body []byte, headers http.Header) error
	// OnAuthRetry, if set, is called when a 401/403 is received.
	// The current request context is passed so that refresh operations
	// use a live context rather than a potentially canceled one.
	// If it returns nil (success), the request is retried with fresh credentials.
	// If it returns an error, that error is returned to the caller.
	OnAuthRetry func(ctx context.Context) error

	responseStateMu         sync.Mutex
	responseStateGeneration uint64
}

// ResponsesRequest follows the Open Responses spec
type ResponsesRequest struct {
	Model                           string               `json:"model"`
	Instructions                    string               `json:"instructions,omitempty"` // System instructions (alternative to developer-role input items)
	Input                           []ResponsesInputItem `json:"input"`
	Messages                        []Message            `json:"-"`               // Optional raw transcript for lazy input materialization
	ExtractInstructionsFromMessages bool                 `json:"-"`               // When lazily materializing Input from Messages, omit system messages because they are sent via Instructions.
	Tools                           []any                `json:"tools,omitempty"` // Can contain ResponsesTool or ResponsesWebSearchTool
	ToolChoice                      any                  `json:"tool_choice,omitempty"`
	ParallelToolCalls               *bool                `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens                 int                  `json:"max_output_tokens,omitempty"`
	Temperature                     *float64             `json:"temperature,omitempty"`
	TopP                            *float64             `json:"top_p,omitempty"`
	Reasoning                       *ResponsesReasoning  `json:"reasoning,omitempty"`
	Include                         []string             `json:"include,omitempty"`
	PromptCacheKey                  string               `json:"prompt_cache_key,omitempty"`
	Store                           *bool                `json:"store,omitempty"`
	Generate                        *bool                `json:"generate,omitempty"` // WebSocket warmup support; omitted for normal HTTP/WS requests
	Stream                          bool                 `json:"stream"`
	PreviousResponseID              string               `json:"previous_response_id,omitempty"`
	ServiceTier                     string               `json:"service_tier,omitempty"`
	SessionID                       string               `json:"-"`
}

// ResponsesWebSearchTool represents the web search tool for OpenAI
type ResponsesWebSearchTool struct {
	Type string `json:"type"` // "web_search_preview"
}

// ResponsesImageGenerationTool represents the built-in image_generation tool.
// See https://platform.openai.com/docs/guides/tools-image-generation
type ResponsesImageGenerationTool struct {
	Type         string `json:"type"`                    // "image_generation"
	OutputFormat string `json:"output_format,omitempty"` // "png", "jpeg", "webp"
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
	Detail   string `json:"detail,omitempty"`
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
	// For image_generation_call
	Result        string `json:"result,omitempty"`         // base64-encoded image payload
	RevisedPrompt string `json:"revised_prompt,omitempty"` // model's revised prompt
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
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}

type responsesError struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
	Message string `json:"message"`
}

type responsesAPIEventError struct {
	Status   int
	APIError *responsesError
}

func (e *responsesAPIEventError) Error() string {
	if e == nil || e.APIError == nil {
		return "Responses API error: unknown error"
	}
	if e.APIError.Code != "" {
		return fmt.Sprintf("Responses API error (%s): %s", e.APIError.Code, e.APIError.Message)
	}
	return fmt.Sprintf("Responses API error: %s", e.APIError.Message)
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

func buildResponsesInputItems(messages []Message) []ResponsesInputItem {
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

// BuildResponsesInput converts []Message to Open Responses input format.
func BuildResponsesInput(messages []Message) []ResponsesInputItem {
	return buildResponsesInputItems(sanitizeToolHistory(messages))
}

// BuildResponsesContinuationInput converts only the newest turn payload needed for a
// server-state continuation. Unlike BuildResponsesInput it intentionally skips
// whole-transcript tool-history sanitization so trailing tool results can be sent
// back against server-side conversation state without rebuilding earlier turns.
func BuildResponsesContinuationInput(messages []Message) []ResponsesInputItem {
	messages = FilterConversationMessages(messages)
	if len(messages) == 0 {
		return nil
	}

	start := len(messages)
	sawToolResult := false
	for start > 0 {
		msg := messages[start-1]
		if msg.Role == RoleUser && !sawToolResult {
			start--
			continue
		}
		if msg.Role == RoleTool {
			sawToolResult = true
			start--
			continue
		}
		break
	}
	if !sawToolResult {
		start = 0
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == RoleUser {
				start = i
				break
			}
		}
	}

	return buildResponsesInputItems(messages[start:])
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
				imageParts := []ResponsesContentPart{{Type: "input_image", ImageURL: dataURL, Detail: imageDetail(part.ImageData.Detail)}}
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
		strict := spec.Strict
		params := openAIParametersFromToolSchema(spec.Schema, strict)
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

func (c *ResponsesClient) responseState() (lastResponseID string, generation uint64) {
	c.responseStateMu.Lock()
	defer c.responseStateMu.Unlock()
	return c.LastResponseID, c.responseStateGeneration
}

func (c *ResponsesClient) clearLastResponseIDIfGeneration(generation uint64) {
	c.responseStateMu.Lock()
	defer c.responseStateMu.Unlock()
	if c.responseStateGeneration != generation {
		return
	}
	c.LastResponseID = ""
}

func (c *ResponsesClient) setLastResponseIDIfGeneration(generation uint64, responseID string) {
	c.responseStateMu.Lock()
	defer c.responseStateMu.Unlock()
	if c.responseStateGeneration != generation {
		return
	}
	c.LastResponseID = responseID
}

func cloneResponsesClientFreshConversation(c *ResponsesClient) *ResponsesClient {
	if c == nil {
		return nil
	}

	var extraHeaders map[string]string
	if c.ExtraHeaders != nil {
		extraHeaders = make(map[string]string, len(c.ExtraHeaders))
		for key, value := range c.ExtraHeaders {
			extraHeaders[key] = value
		}
	}

	return &ResponsesClient{
		BaseURL:                 c.BaseURL,
		GetAuthHeader:           c.GetAuthHeader,
		ExtraHeaders:            extraHeaders,
		HTTPClient:              c.HTTPClient,
		DisableServerState:      c.DisableServerState,
		UseWebSocket:            c.UseWebSocket,
		WebSocketServerState:    c.WebSocketServerState,
		WebSocketURL:            c.WebSocketURL,
		WebSocketConnectTimeout: c.WebSocketConnectTimeout,
		WebSocketWriteTimeout:   c.WebSocketWriteTimeout,
		WebSocketIdleTimeout:    c.WebSocketIdleTimeout,
		HandleError:             c.HandleError,
		OnAuthRetry:             c.OnAuthRetry,
	}
}

// Stream makes a streaming request to the Responses API and returns events via a Stream
func (c *ResponsesClient) Stream(ctx context.Context, req ResponsesRequest, debugRaw bool) (Stream, error) {
	fullInput := req.Input
	fullInputBuilt := req.Input != nil
	buildFullInput := func() []ResponsesInputItem {
		if fullInputBuilt {
			return fullInput
		}
		if req.ExtractInstructionsFromMessages {
			_, fullInput = BuildResponsesInputWithInstructions(req.Messages)
		} else {
			fullInput = BuildResponsesInput(req.Messages)
		}
		fullInputBuilt = true
		return fullInput
	}

	continuationInput := []ResponsesInputItem(nil)
	continuationBuilt := false
	buildContinuationInput := func() []ResponsesInputItem {
		if continuationBuilt {
			return continuationInput
		}
		if len(req.Messages) > 0 {
			continuationInput = BuildResponsesContinuationInput(req.Messages)
		} else {
			continuationInput = filterToNewInput(buildFullInput())
		}
		continuationBuilt = true
		return continuationInput
	}

	lastResponseID, responseStateGeneration := c.responseState()

	wsReq := req
	httpPayload := req
	if lastResponseID != "" {
		if c.websocketServerStateEnabled() {
			wsReq.PreviousResponseID = lastResponseID
			wsReq.Input = buildContinuationInput()
		} else {
			wsReq.Input = buildFullInput()
		}
		if !c.DisableServerState {
			httpPayload.PreviousResponseID = lastResponseID
			httpPayload.Input = buildContinuationInput()
		} else {
			httpPayload.Input = buildFullInput()
		}
	} else {
		wsReq.Input = buildFullInput()
		httpPayload.Input = fullInput
	}

	if c.UseWebSocket && !c.websocketDisabled {
		buildWebSocketFallbacks := func(nextAttempt int) []responsesWebSocketFallback {
			fallbacks := make([]responsesWebSocketFallback, 0, responsesWebSocketMaxAttempts-nextAttempt+2)
			for attempt := nextAttempt; attempt <= responsesWebSocketMaxAttempts; attempt++ {
				attempt := attempt
				wait := responsesWebSocketBackoff(attempt - 1)
				fallbacks = append(fallbacks, responsesWebSocketFallback{
					retry: &Event{
						Type:             EventRetry,
						RetryAttempt:     attempt,
						RetryMaxAttempts: responsesWebSocketMaxAttempts,
						RetryWaitSecs:    wait.Seconds(),
					},
					open: func() (Stream, error) {
						c.closeWebSocket()
						if debugRaw {
							DebugRawSection(debugRaw, "Responses WebSocket Retry", fmt.Sprintf("stream failed before emitting events; retrying WebSocket attempt %d/%d after %s", attempt, responsesWebSocketMaxAttempts, wait))
						}
						if err := sleepWithContext(ctx, wait); err != nil {
							return nil, err
						}
						return c.streamWebSocketPrepared(ctx, wsReq, buildFullInput, debugRaw, responseStateGeneration)
					},
				})
			}
			fallbacks = append(fallbacks, responsesWebSocketFallback{
				open: func() (Stream, error) {
					c.websocketDisabled = true
					c.closeWebSocket()
					if debugRaw {
						DebugRawSection(debugRaw, "Responses WebSocket Fallback", "stream failed before emitting events")
					}
					return c.streamHTTPPrepared(ctx, httpPayload, buildFullInput, responseStateGeneration, debugRaw)
				},
			})
			return fallbacks
		}

		stream, err := c.streamWebSocketPrepared(ctx, wsReq, buildFullInput, debugRaw, responseStateGeneration)
		if err == nil {
			return &responsesWebSocketFallbackStream{
				current:   stream,
				fallbacks: buildWebSocketFallbacks(2),
			}, nil
		}
		lastErr := err
		for attempt := 2; attempt <= responsesWebSocketMaxAttempts; attempt++ {
			wait := responsesWebSocketBackoff(attempt - 1)
			c.closeWebSocket()
			if debugRaw {
				DebugRawSection(debugRaw, "Responses WebSocket Retry", fmt.Sprintf("initial stream setup failed: %v; retrying WebSocket attempt %d/%d after %s", lastErr, attempt, responsesWebSocketMaxAttempts, wait))
			}
			if err := sleepWithContext(ctx, wait); err != nil {
				return nil, err
			}
			stream, err = c.streamWebSocketPrepared(ctx, wsReq, buildFullInput, debugRaw, responseStateGeneration)
			if err == nil {
				return &responsesWebSocketFallbackStream{
					current:   stream,
					fallbacks: buildWebSocketFallbacks(attempt + 1),
				}, nil
			}
			lastErr = err
		}
		c.websocketDisabled = true
		c.closeWebSocket()
		if debugRaw {
			DebugRawSection(debugRaw, "Responses WebSocket Fallback", lastErr.Error())
		}
	}

	return c.streamHTTPPrepared(ctx, httpPayload, buildFullInput, responseStateGeneration, debugRaw)
}

func (c *ResponsesClient) streamHTTPPrepared(ctx context.Context, httpPayload ResponsesRequest, buildFullInput func() []ResponsesInputItem, responseStateGeneration uint64, debugRaw bool) (Stream, error) {
	body, err := json.Marshal(httpPayload)
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
	if httpPayload.SessionID != "" {
		httpReq.Header.Set("session_id", httpPayload.SessionID)
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

		// Check for previous_response_id not found error.
		if resp.StatusCode == http.StatusNotFound && httpPayload.PreviousResponseID != "" {
			// Clear state and retry with full history
			c.clearLastResponseIDIfGeneration(responseStateGeneration)
			retryPayload := httpPayload
			retryPayload.PreviousResponseID = ""
			retryPayload.Input = buildFullInput()
			// Re-marshal without previous_response_id
			body, err = json.Marshal(retryPayload)
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
			if retryPayload.SessionID != "" {
				httpReq.Header.Set("session_id", retryPayload.SessionID)
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
				if httpPayload.SessionID != "" {
					httpReq.Header.Set("session_id", httpPayload.SessionID)
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
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		defer resp.Body.Close()

		reader := bufio.NewReader(resp.Body)

		var lastEventType string
		var eventData []byte
		handler := newResponsesStreamEventHandler(client, responseStateGeneration, debugRaw, "Responses API SSE", !client.DisableServerState)
		sawTerminal := false

		flushEvent := func() (bool, error) {
			if len(eventData) == 0 {
				lastEventType = ""
				return false, nil
			}

			data := eventData
			eventType := lastEventType
			lastEventType = ""

			stop, err := handler.HandleJSONEvent(data, eventType, send)
			if stop {
				sawTerminal = true
			}
			eventData = data[:0]
			return stop, err
		}

		for {
			line, eof, err := readSSELineBytes(reader)
			if err != nil {
				return fmt.Errorf("Responses API streaming error: %w", err)
			}

			if len(line) == 0 {
				stop, err := flushEvent()
				if err != nil {
					return err
				}
				if stop || eof {
					break
				}
				continue
			}

			if i := bytes.IndexByte(line, ':'); i >= 0 {
				field, value := line[:i], line[i+1:]
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
				}
				if bytes.Equal(field, sseEventField) && len(eventData) > 0 {
					stop, err := flushEvent()
					if err != nil {
						return err
					}
					if stop {
						break
					}
				}
				if bytes.Equal(field, sseDataField) && bytes.Equal(value, sseDoneData) && len(eventData) > 0 {
					stop, err := flushEvent()
					if err != nil {
						return err
					}
					if stop {
						break
					}
				}
				switch {
				case bytes.Equal(field, sseEventField):
					lastEventType = string(value)
				case bytes.Equal(field, sseDataField):
					if len(eventData) > 0 {
						eventData = append(eventData, '\n')
					}
					eventData = append(eventData, value...)
				}
				if bytes.Equal(field, sseDataField) && bytes.Equal(value, sseDoneData) {
					stop, err := flushEvent()
					if err != nil {
						return err
					}
					if stop || eof {
						break
					}
					continue
				}
			}

			if eof {
				stop, err := flushEvent()
				if err != nil {
					return err
				}
				if stop {
					break
				}
				break
			}
		}

		if !sawTerminal {
			if err := handler.FinishIncomplete(send); err != nil {
				return &StreamIncompleteError{Transport: "Responses API SSE", Terminal: "response.completed or [DONE]", Err: err}
			}
			return &StreamIncompleteError{Transport: "Responses API SSE", Terminal: "response.completed or [DONE]"}
		}

		return handler.Finish(send)
	}), nil
}

type NonRecoverableStreamError struct {
	Err error
}

func (e *NonRecoverableStreamError) Error() string {
	if e == nil || e.Err == nil {
		return "Responses WebSocket disconnected after partial output; automatic retry is unsafe without rewinding. Partial response preserved."
	}
	return fmt.Sprintf("Responses WebSocket disconnected after partial output; automatic retry is unsafe without rewinding. Partial response preserved. Original error: %v", e.Err)
}

func (e *NonRecoverableStreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type responsesWebSocketFallback struct {
	retry *Event
	open  func() (Stream, error)
}

type responsesWebSocketFallbackStream struct {
	current         Stream
	fallbacks       []responsesWebSocketFallback
	emitted         bool
	pendingFallback *responsesWebSocketFallback
}

func responsesWebSocketBackoff(retryAttempt int) time.Duration {
	if retryAttempt < 1 {
		retryAttempt = 1
	}
	backoff := responsesWebSocketBaseBackoff << (retryAttempt - 1)
	if backoff > responsesWebSocketMaxBackoff {
		return responsesWebSocketMaxBackoff
	}
	return backoff
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *responsesWebSocketFallbackStream) Recv() (Event, error) {
	if s.pendingFallback != nil {
		return s.openPendingFallback()
	}
	event, err := s.current.Recv()
	if err != nil {
		if err == io.EOF || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || len(s.fallbacks) == 0 {
			return event, err
		}
		if s.emitted {
			return event, &NonRecoverableStreamError{Err: err}
		}
		return s.switchToNextFallback(err)
	}
	if event.Type == EventError {
		if !s.emitted && len(s.fallbacks) > 0 {
			return s.switchToNextFallback(event.Err)
		}
	}
	if event.Type != EventHeartbeat && event.Type != EventRetry {
		s.emitted = true
	}
	return event, nil
}

func (s *responsesWebSocketFallbackStream) openPendingFallback() (Event, error) {
	fb := s.pendingFallback
	s.pendingFallback = nil
	stream, err := fb.open()
	if err != nil {
		return s.switchToNextFallback(err)
	}
	s.current = stream
	return s.Recv()
}

func (s *responsesWebSocketFallbackStream) switchToNextFallback(previousErr error) (Event, error) {
	_ = s.current.Close()
	lastErr := previousErr
	if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
		return Event{}, lastErr
	}
	for len(s.fallbacks) > 0 {
		fb := s.fallbacks[0]
		s.fallbacks = s.fallbacks[1:]
		if fb.retry != nil {
			s.pendingFallback = &fb
			return *fb.retry, nil
		}
		stream, err := fb.open()
		if err != nil {
			lastErr = err
			if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
				return Event{}, lastErr
			}
			continue
		}
		s.current = stream
		return s.Recv()
	}
	return Event{}, lastErr
}

func (s *responsesWebSocketFallbackStream) Close() error {
	return s.current.Close()
}

// ResetConversation clears server state (called on /clear or new conversation)
func (c *ResponsesClient) ResetConversation() {
	c.closeWebSocket()
	c.responseStateMu.Lock()
	defer c.responseStateMu.Unlock()
	c.responseStateGeneration++
	c.LastResponseID = ""
	c.websocketDisabled = false
	c.wsLastRequest = nil
	c.wsLastResponseItems = nil
}

func (c *ResponsesClient) websocketServerStateEnabled() bool {
	return c.WebSocketServerState || !c.DisableServerState
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
		if item.Type == "message" && item.Role == "user" && !sawToolOutput {
			// A trailing user message may be the next user turn or rich/image
			// content appended after a tool result.
			start--
			continue
		}
		if item.Type == "function_call_output" {
			sawToolOutput = true
			start--
			continue
		}
		// If we have already seen a trailing tool output, a user message before
		// it is part of the prior prompt, not new incremental input.
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

func (s *responsesToolState) Validate() error {
	for _, outputIndex := range s.order {
		state := s.calls[outputIndex]
		if state == nil {
			continue
		}
		if !state.finished {
			return fmt.Errorf("Responses API stream ended before tool call %d completed", outputIndex)
		}
		if strings.TrimSpace(state.callID) == "" {
			return fmt.Errorf("Responses API stream missing call_id for tool call %d", outputIndex)
		}
		args := strings.TrimSpace(state.args.String())
		if args == "" {
			return fmt.Errorf("Responses API stream missing arguments for tool call %d", outputIndex)
		}
		if !json.Valid([]byte(args)) {
			return fmt.Errorf("Responses API stream invalid arguments for tool call %d", outputIndex)
		}
	}
	return nil
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
