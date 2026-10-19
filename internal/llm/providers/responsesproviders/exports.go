package responsesproviders

import (
	"context"
	"net/http"

	"github.com/samsaffron/term-llm/internal/credentials"
)

// Exports for the parent llm package's model registry, completion, usage and
// context-window code, which still consult these providers' private caches
// and constants.

const (
	// ChatGPTOriginator is the originator header value sent to the ChatGPT backend.
	ChatGPTOriginator = chatGPTOriginator
	// OpenCodeGoBaseURL is the default OpenCode Go gateway base URL.
	OpenCodeGoBaseURL = opencodeGoBaseURL
	// OpenCodeGoRefreshTimeout bounds interactive OpenCode Go catalog refreshes.
	OpenCodeGoRefreshTimeout = opencodeGoRefreshTimeout
	// GrokProxyCompatibilityVersion is the Grok CLI proxy version sent with requests.
	GrokProxyCompatibilityVersion = grokProxyCompatibilityVersion
	// GrokUserAgent is the User-Agent sent to Grok backends.
	GrokUserAgent = grokUserAgent
)

// Grok46ReasoningEfforts returns the static Grok 4.6 effort list.
func Grok46ReasoningEfforts() []string { return grok46ReasoningEfforts }

func ChatGPTUserAgent() string { return chatGPTUserAgent() }

// ChatGPTHTTPClient returns the HTTP client used for ChatGPT backend calls.
func ChatGPTHTTPClient() *http.Client { return chatGPTHTTPClient }

func CachedChatGPTModelFacts() ([]ModelInfo, bool, error) { return cachedChatGPTModelFacts() }

func ChatGPTModelServiceTier(model string) (string, string) { return chatGPTModelServiceTier(model) }

func ChatGPTCachedModelInfo(model string) (ModelInfo, bool) { return chatGPTCachedModelInfo(model) }

func ChatGPTContextModelID(provider, model string) string {
	return chatGPTContextModelID(provider, model)
}

func ResolveChatGPTContext(provider string, info ModelInfo) ModelInfo {
	return resolveChatGPTContext(provider, info)
}

func CopilotCachedInputLimit(model string) int { return copilotCachedInputLimit(model) }

func GrokCachedInputLimit(model string) int { return grokCachedInputLimit(model) }

func GrokCachedReasoningEfforts(model string) ([]string, bool) {
	return grokCachedReasoningEfforts(model)
}

func StaticGrokReasoningEfforts(id string) []string { return staticGrokReasoningEfforts(id) }

func RefreshGrokSession(ctx context.Context, creds *credentials.GrokCredentials, force bool) error {
	return refreshGrokSession(ctx, creds, force)
}

func OpenCodeGoCachedInputLimit(model string) int { return opencodeGoCachedInputLimit(model) }

func OpenCodeGoCachedReasoningEfforts(model string) []string {
	return opencodeGoCachedReasoningEfforts(model)
}

func GetCachedOpenCodeGoModelsForScope(apiKey, baseURL string) []string {
	return getCachedOpenCodeGoModelsForScope(apiKey, baseURL)
}

// PrimeOpenCodeGoCatalogScope loads the credential-scoped OpenCode Go disk
// cache so later provider-aware lookups share its metadata.
func PrimeOpenCodeGoCatalogScope(apiKey, baseURL string) {
	primeOpenCodeGoCatalogScope(apiKey, baseURL)
}

func ApplyOpenCodeGoAttributionHeaders(header http.Header, sessionID string) {
	applyOpenCodeGoAttributionHeaders(header, sessionID)
}

// EffectiveInputLimit is the exported form of the runtime input-limit probe
// the root llm engine looks for on providers.
func (p *ChatGPTProvider) EffectiveInputLimit(ctx context.Context, model string) (int, error) {
	return p.effectiveInputLimit(ctx, model)
}

// EffectiveInputLimitForProvider resolves the limit under the configured
// provider name's context policy.
func (p *ChatGPTProvider) EffectiveInputLimitForProvider(ctx context.Context, provider, model string) (int, error) {
	return p.effectiveInputLimitForProvider(ctx, provider, model)
}

// IsolateHelperConversation is the exported form of the root llm package's
// helperConversationIsolator seam.
func (p *ChatGPTProvider) IsolateHelperConversation() Provider { return p.isolateHelperConversation() }

// IsolateHelperConversation is the exported form of the root llm package's
// helperConversationIsolator seam.
func (p *CopilotProvider) IsolateHelperConversation() Provider { return p.isolateHelperConversation() }

// IsolateHelperConversation is the exported form of the root llm package's
// helperConversationIsolator seam.
func (p *GrokProvider) IsolateHelperConversation() Provider { return p.isolateHelperConversation() }

// IsolateHelperConversation is the exported form of the root llm package's
// helperConversationIsolator seam.
func (p *OpenAIProvider) IsolateHelperConversation() Provider { return p.isolateHelperConversation() }

// ForkHelperConversation is the exported form of the suffix-only handover fork seam.
func (p *OpenAIProvider) ForkHelperConversation() (Provider, bool) {
	return p.forkHelperConversation()
}
