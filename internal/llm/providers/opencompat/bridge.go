package opencompat

import (
	"context"
	"net/http"
)

// Exported names for wire types and helpers that the root llm package still
// shares with the Copilot, Zen, OpenCode Go and OpenAI providers.
type (
	OAIChatRequest    = oaiChatRequest
	OAIChatResponse   = oaiChatResponse
	OAIStreamOptions  = oaiStreamOptions
	OAIModelsResponse = oaiModelsResponse
	OAIToolCall       = oaiToolCall
	CompatToolState   = compatToolState
)

type (
	OAIMessage = oaiMessage
	OAITool    = oaiTool
)

func BuildCompatMessages(messages []Message) []OAIMessage { return buildCompatMessages(messages) }

func BuildCompatTools(specs []ToolSpec) ([]OAITool, error) { return buildCompatTools(specs) }

func BuildCompatToolChoice(choice ToolChoice) interface{} { return buildCompatToolChoice(choice) }

func NewCompatToolState() *CompatToolState { return newCompatToolState() }

func OpenRouterCachedInputLimit(model string) int { return openRouterCachedInputLimit(model) }

func VeniceCachedInputLimit(model string) int { return veniceCachedInputLimit(model) }

// BaseURL returns the normalized base URL requests are issued against.
func (p *OpenAICompatProvider) BaseURL() string { return p.baseURL }

// Model returns the provider's resolved model name.
func (p *OpenAICompatProvider) Model() string { return p.model }

// SetNoStreamOptions controls whether stream_options is omitted from requests.
func (p *OpenAICompatProvider) SetNoStreamOptions(v bool) { p.noStreamOptions = v }

// SetVLLMThinkingParam overrides the vLLM chat_template_kwargs thinking key.
func (p *OpenAICompatProvider) SetVLLMThinkingParam(v string) { p.vllmThinkingParam = v }

// SetRequestHeaders installs a per-request header supplier.
func (p *OpenAICompatProvider) SetRequestHeaders(fn func(ctx context.Context) map[string]string) {
	p.requestHeaders = fn
}

// MakeChatRequest issues a chat completions request.
func (p *OpenAICompatProvider) MakeChatRequest(ctx context.Context, req OAIChatRequest) (*http.Response, error) {
	return p.makeChatRequest(ctx, req)
}

// MakeRequest issues an authenticated request relative to the base URL.
func (p *OpenAICompatProvider) MakeRequest(ctx context.Context, method, endpoint string, body []byte) (*http.Response, error) {
	return p.makeRequest(ctx, method, endpoint, body)
}

// APIKey returns the configured API key.
func (p *OpenAICompatProvider) APIKey() string { return p.apiKey }

// Model returns the xAI provider's resolved model name.
func (p *XAIProvider) Model() string { return p.model }
