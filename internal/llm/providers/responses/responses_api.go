package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxResponsesAPIErrorBodyBytes = 64 * 1024

var truncatedResponsesAPIErrorBodySuffix = []byte("\n... response body truncated")

const (
	// Match Codex's default stream retry budget: five reconnect attempts after
	// the initial WebSocket stream attempt. Keep the derived attempts constant so
	// existing EventRetry attempt/max fields continue to describe attempt numbers.
	responsesWebSocketMaxRetries  = 5
	responsesWebSocketMaxAttempts = responsesWebSocketMaxRetries + 1
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
	WebSocketServerState       bool
	WebSocketURL               string
	WebSocketPoolKey           string
	WebSocketConnectTimeout    time.Duration
	WebSocketWriteTimeout      time.Duration
	WebSocketIdleTimeout       time.Duration
	WebSocketFirstEventTimeout time.Duration
	WebSocketParkedTimeout     time.Duration
	websocketDisabled          bool
	wsMu                       sync.Mutex
	wsConn                     *responsesWebSocketConnection
	wsConnSessionID            string
	wsConnBetaHeader           string
	wsLastRequest              *ResponsesRequest
	wsContinuationBaseline     *responsesWebSocketContinuationBaseline
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
	responseStateSessionID  string
}

// ResponsesRequest follows the Open Responses spec
type ResponsesRequest struct {
	Model                           string                       `json:"model"`
	Instructions                    string                       `json:"instructions,omitempty"` // System instructions (alternative to developer-role input items)
	Input                           []ResponsesInputItem         `json:"input"`
	Messages                        []Message                    `json:"-"`               // Optional raw transcript for lazy input materialization
	ExtractInstructionsFromMessages bool                         `json:"-"`               // When lazily materializing Input from Messages, omit system messages because they are sent via Instructions.
	IncludeDeveloperInContinuation  bool                         `json:"-"`               // Include the developer policy immediately preceding the latest user continuation.
	Tools                           []any                        `json:"tools,omitempty"` // Can contain ResponsesTool or ResponsesWebSearchTool
	ToolChoice                      any                          `json:"tool_choice,omitempty"`
	ParallelToolCalls               *bool                        `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens                 int                          `json:"max_output_tokens,omitempty"`
	Text                            *ResponsesText               `json:"text,omitempty"`
	Temperature                     *float64                     `json:"temperature,omitempty"`
	TopP                            *float64                     `json:"top_p,omitempty"`
	Reasoning                       *ResponsesReasoning          `json:"reasoning,omitempty"`
	MultiAgent                      *ResponsesMultiAgent         `json:"multi_agent,omitempty"`
	PromptCacheOptions              *ResponsesPromptCacheOptions `json:"prompt_cache_options,omitempty"`
	Include                         []string                     `json:"include,omitempty"`
	PromptCacheKey                  string                       `json:"prompt_cache_key,omitempty"`
	Store                           *bool                        `json:"store,omitempty"`
	Generate                        *bool                        `json:"generate,omitempty"` // WebSocket warmup support; omitted for normal HTTP/WS requests
	Stream                          bool                         `json:"stream"`
	StreamOptions                   *ResponsesStreamOptions      `json:"stream_options,omitempty"`
	PreviousResponseID              string                       `json:"previous_response_id,omitempty"`
	ServiceTier                     string                       `json:"service_tier,omitempty"`
	SessionID                       string                       `json:"-"`
	ForceHTTP                       bool                         `json:"-"` // Bypass WebSocket for request features that require HTTP/SSE.
	ForceWebSocket                  bool                         `json:"-"` // Require WebSocket and never downgrade this request to HTTP/SSE.
	ExtraHeaders                    map[string]string            `json:"-"` // Request-scoped headers (combined with client headers).
	FileUploadPolicy                FilePolicy                   `json:"-"`
}

// ResponsesStreamOptions contains streaming delivery options for the Responses API.
type ResponsesStreamOptions struct {
	ReasoningSummaryDelivery string `json:"reasoning_summary_delivery,omitempty"`
}

func (r ResponsesRequest) suppressReasoningSummaryDeltas() bool {
	if r.StreamOptions == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.StreamOptions.ReasoningSummaryDelivery), "sequential_cutoff")
}

// ResponsesWebSearchTool represents the web search tool for OpenAI
type ResponsesWebSearchTool struct {
	Type string `json:"type"` // "web_search" or "web_search_preview"
	// ExternalWebAccess=false restricts the tool to the cached index. Used when
	// the declaration only exists to satisfy replay validation.
	ExternalWebAccess *bool `json:"external_web_access,omitempty"`
}

// ResponsesToolSearchTool is the provider wire shape for native
// client-executed tool discovery. It is deliberately not represented in ToolSpec.
type ResponsesToolSearchTool struct {
	Type        string                 `json:"type"`
	Execution   string                 `json:"execution"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ResponsesImageGenerationTool represents the built-in image_generation tool.
// See https://platform.openai.com/docs/guides/tools-image-generation
type ResponsesImageGenerationTool struct {
	Type         string `json:"type"`                    // "image_generation"
	OutputFormat string `json:"output_format,omitempty"` // "png", "jpeg", "webp"
}

// ResponsesInputItem represents an input item in the Open Responses format
type ResponsesInputItem struct {
	Raw           json.RawMessage `json:"-"` // Exact provider output item for stateless replay.
	Type          string          `json:"type"`
	Role          string          `json:"role,omitempty"`
	Content       interface{}     `json:"content,omitempty"` // string or []ResponsesContentPart
	Phase         string          `json:"phase,omitempty"`
	Agent         string          `json:"agent,omitempty"`
	Caller        string          `json:"caller,omitempty"`
	Program       string          `json:"program,omitempty"`
	ProgramOutput string          `json:"program_output,omitempty"`
	Fingerprint   string          `json:"fingerprint,omitempty"`
	// For reasoning type
	ID               string                     `json:"id,omitempty"`
	EncryptedContent string                     `json:"encrypted_content,omitempty"`
	Summary          *responsesReasoningSummary `json:"summary,omitempty"`
	// For function_call type
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// For function_call_output type
	Output string `json:"output,omitempty"`
}

func (i ResponsesInputItem) MarshalJSON() ([]byte, error) {
	if len(i.Raw) > 0 {
		return append([]byte(nil), i.Raw...), nil
	}
	type wire ResponsesInputItem
	if i.Type == "function_call_output" {
		// The Responses API requires output even when the tool returned an empty
		// string. The field is otherwise omitted to avoid sending it on unrelated
		// input item types.
		return json.Marshal(struct {
			wire
			Output string `json:"output"`
		}{
			wire:   wire(i),
			Output: i.Output,
		})
	}
	return json.Marshal(wire(i))
}

// ResponsesContentPart represents a content part (text, image, or file).
type ResponsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"` // Plain URL string for Responses API (not object)
	Detail   string `json:"detail,omitempty"`
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
}

// ResponsesTool represents a tool definition in Open Responses format.
// Output schemas are intentionally omitted: tool results carry ordinary content,
// not guaranteed schema-conforming JSON (including MCP results and tool errors).
// The same restriction applies to ResponsesNamespaceFunctionTool.
type ResponsesTool struct {
	Type           string                 `json:"type"`
	Name           string                 `json:"name"`
	Description    string                 `json:"description,omitempty"`
	Parameters     map[string]interface{} `json:"parameters"`
	Strict         bool                   `json:"strict,omitempty"`
	AllowedCallers []string               `json:"allowed_callers,omitempty"`
}

// ResponsesNamespace is the native Responses namespace shape returned from a
// client-executed tool_search. Only individually selected children are included.
type ResponsesNamespace struct {
	Type        string                           `json:"type"`
	Name        string                           `json:"name"`
	Description string                           `json:"description,omitempty"`
	Tools       []ResponsesNamespaceFunctionTool `json:"tools"`
}

type ResponsesNamespaceFunctionTool struct {
	Type           string                 `json:"type"`
	Name           string                 `json:"name"`
	Description    string                 `json:"description,omitempty"`
	Parameters     map[string]interface{} `json:"parameters"`
	Strict         bool                   `json:"strict,omitempty"`
	AllowedCallers []string               `json:"allowed_callers,omitempty"`
	DeferLoading   bool                   `json:"defer_loading"`
}

type ResponsesText struct {
	Verbosity string `json:"verbosity,omitempty"`
}

// ResponsesReasoning configures GPT-5.6 reasoning execution.
type ResponsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Context string `json:"context,omitempty"`
}

type ResponsesMultiAgent struct {
	Enabled                bool `json:"enabled"`
	MaxConcurrentSubagents int  `json:"max_concurrent_subagents,omitempty"`
}

type ResponsesPromptCacheOptions struct {
	Mode string `json:"mode,omitempty"`
	TTL  string `json:"ttl,omitempty"`
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
	ID            string `json:"id,omitempty"`
	CallID        string `json:"call_id,omitempty"`
	Name          string `json:"name,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
	Arguments     string `json:"arguments,omitempty"`
	Caller        string `json:"caller,omitempty"`
	Phase         string `json:"phase,omitempty"`
	Agent         string `json:"agent,omitempty"`
	Program       string `json:"program,omitempty"`
	ProgramOutput string `json:"program_output,omitempty"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	// For image_generation_call
	Result        string `json:"result,omitempty"`         // base64-encoded image payload
	RevisedPrompt string `json:"revised_prompt,omitempty"` // model's revised prompt
	// For web_search_call. Action is kept raw because other output item types
	// (notably image_generation_call) may use the same field with a string value.
	Status string          `json:"status,omitempty"`
	Action json.RawMessage `json:"action,omitempty"`
}

type responsesWebSearchAction struct {
	Type    string `json:"type,omitempty"`
	Query   string `json:"query,omitempty"`
	URL     string `json:"url,omitempty"`
	Pattern string `json:"pattern,omitempty"`
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
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
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

func responsesAPIErrorMessage(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if apiError, ok := payload["error"].(map[string]any); ok {
		if message, ok := apiError["message"].(string); ok && strings.TrimSpace(message) != "" {
			return strings.TrimSpace(message)
		}
	}
	message, _ := payload["message"].(string)
	return strings.TrimSpace(message)
}

func responsesAPIErrorDetail(body []byte) string {
	if message := responsesAPIErrorMessage(body); message != "" {
		return message
	}
	return string(body)
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
