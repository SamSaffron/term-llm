package responses

import (
	"context"
	"encoding/json"

	"github.com/samsaffron/term-llm/internal/config"
)

// HistoryFacts summarizes transcript properties that gate Responses requests.
type HistoryFacts = responsesHistoryFacts

// ScanHistory walks the transcript once and reports replay/discovery facts.
func ScanHistory(messages []Message) HistoryFacts { return scanResponsesHistory(messages) }

// GuardReplayedWebSearch makes a request with replayed web_search_call history acceptable.
func GuardReplayedWebSearch(req *ResponsesRequest, facts HistoryFacts) {
	guardReplayedWebSearch(req, facts)
}

// MergeOptions overlays per-request Responses options on provider defaults.
func MergeOptions(defaults ResponsesOptions, override *ResponsesOptions, ephemeral bool) ResponsesOptions {
	return mergeResponsesOptions(defaults, override, ephemeral)
}

// ValidateOptions validates and normalizes Responses options for a provider/model.
func ValidateOptions(provider, model string, opts *ResponsesOptions, tools []ToolSpec) (ResponsesOptions, error) {
	return validateResponsesOptions(provider, model, opts, tools)
}

// OptionsFromConfig converts config-file Responses settings to ResponsesOptions.
func OptionsFromConfig(cfg config.ResponsesConfig) ResponsesOptions {
	return responsesOptionsFromConfig(cfg)
}

// ParseModelEffortForProvider splits a provider model into base model and effort.
func ParseModelEffortForProvider(provider, model string) (string, string) {
	return parseModelEffortForProvider(provider, model)
}

// CloneClientFreshConversation clones a client with an independent response chain.
func CloneClientFreshConversation(c *ResponsesClient) *ResponsesClient {
	return cloneResponsesClientFreshConversation(c)
}

// CloneClientForkConversation clones a client that branches from the live response.
func CloneClientForkConversation(c *ResponsesClient) (*ResponsesClient, bool) {
	return cloneResponsesClientForkConversation(c)
}

// WithWebSocketContinuationLifetime records ctx as the lifetime that owns
// WebSocket continuation state started under derived contexts.
func WithWebSocketContinuationLifetime(ctx context.Context) context.Context {
	return withResponsesWebSocketContinuationLifetime(ctx)
}

// OutputItem is the decoded shape of a Responses output item.
type OutputItem = responsesOutputItem

// WebSocketBetaHeader is the OpenAI-Beta value that enables Responses WebSockets.
const WebSocketBetaHeader = responsesWebSocketBetaHeader

// MaxPreferredTextEmbedBytes bounds uploads preferred as inline text.
const MaxPreferredTextEmbedBytes = maxPreferredTextEmbedBytes

// ComposeBetaHeader appends addition to a comma-separated OpenAI-Beta value once.
func ComposeBetaHeader(existing, addition string) string {
	return composeBetaHeader(existing, addition)
}

// BuildAssistantItems converts assistant message parts to Responses input items.
func BuildAssistantItems(parts []Part) []ResponsesInputItem {
	return buildResponsesAssistantItems(parts)
}

// TrimContinuationToDiscoveryOutput keeps the input items a discovery continuation needs.
func TrimContinuationToDiscoveryOutput(input []ResponsesInputItem) []ResponsesInputItem {
	return trimResponsesContinuationToDiscoveryOutput(input)
}

// IsWebSearchCall reports whether raw is a hosted web_search_call output item.
func IsWebSearchCall(raw json.RawMessage) bool { return isResponsesWebSearchCall(raw) }

// ResponseStateSessionID returns the session that owns the tracked response chain.
func (c *ResponsesClient) ResponseStateSessionID() string {
	_, _, sessionID := c.responseState()
	return sessionID
}

// SetResponseStateSessionID overrides the tracked response chain's session owner.
func (c *ResponsesClient) SetResponseStateSessionID(sessionID string) {
	c.responseStateMu.Lock()
	defer c.responseStateMu.Unlock()
	c.responseStateSessionID = sessionID
}

// CloseWebSocket closes any pooled WebSocket connection held by the client.
func (c *ResponsesClient) CloseWebSocket() { c.closeWebSocket() }
