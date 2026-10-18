package responsesproviders

import (
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/openaiapi"
)

// Test-only seams for the external integration tests, which exercise these
// providers together with the parent llm package's model registry.

type (
	ChatGPTModelsCacheForTest   = chatGPTModelsCache
	ChatGPTModelInfoForTest     = chatGPTModelInfo
	ChatGPTReasoningLevelsTest  = chatGPTReasoningLevels
	OpenCodeGoModelForTest      = opencodeGoModel
	FakeGrokOAuthAPIForTest     = fakeGrokOAuthAPI
	ChatGPTTranscribeRequestFor = chatGPTTranscribeRequest
)

const (
	ChatGPTModelsClientVersionForTest = chatGPTModelsClientVersion
	GrokDefaultInstructionsForTest    = grokDefaultInstructions
	GrokResponsesURLForTest           = grokResponsesURL
	GrokModelsURLForTest              = grokModelsURL
	OpenCodeGoBaseURLForTest          = opencodeGoBaseURL
	OpenCodeGoProtocolChatForTest     = opencodeGoProtocolChat
	OpenCodeGoProtocolGeminiForTest   = opencodeGoProtocolGemini
)

var (
	GrokHTTPClientForTest    = &grokHTTPClient
	ChatGPTHTTPClientForTest = &chatGPTHTTPClient
	CopilotHTTPClientForTest = &copilotHTTPClient
	ModelsDevURLForTest      = &modelsDevURL
	ZenCatalogURLForTest     = &zenCatalogURL

	SaveChatGPTModelsCacheForTest      = saveChatGPTModelsCache
	LoadChatGPTModelsCacheForTest      = loadChatGPTModelsCache
	WriteCachedOpenCodeGoModelsForTest = writeCachedOpenCodeGoModels
	OpenCodeGoCatalogScopeForTest      = openCodeGoCatalogScope
	NewOpenCodeGoProviderForTest       = newOpenCodeGoProvider
	NewGrokResponsesClientForTest      = newGrokResponsesClient
	IsolateGrokLLMTestEnvForTest       = isolateGrokLLMTestEnv
	GrokSSEForTest                     = grokSSE
	ValidGrokUUIDv4ForTest             = validGrokUUIDv4
	ClearOpenAIEndpointEnvForTest      = clearOpenAIEndpointEnv
	ChatGPTModelServiceTierForTest     = chatGPTModelServiceTier
	ResolveChatGPTContextForTest       = resolveChatGPTContext
	NewOpenCodeGoTestServerForTest     = newOpenCodeGoTestServer
)

func (m chatGPTModelInfo) ToModelInfoForTest() ModelInfo { return m.toModelInfo() }

func ChatGPTModelEffortTierForTest(p *ChatGPTProvider) (model, effort, tier string) {
	return p.model, p.effort, p.serviceTier
}

func SetOpenAIEffortForTest(p *OpenAIProvider, effort string) { p.effort = effort }

func OpenAIResponsesClientForTest(p *OpenAIProvider) *ResponsesClient { return p.responsesClient }

func OpenAIEndpointForTest(p *OpenAIProvider) openaiapi.Endpoint { return p.endpoint }

func GrokModelEffortForTest(p *GrokProvider) (model, effort string) { return p.model, p.effort }

// ResetGrokResponsesClientForTest installs a fresh Grok Responses client that
// carries a stale response ID, which the provider must not send.
func ResetGrokResponsesClientForTest(p *GrokProvider, creds *credentials.GrokCredentials, lastResponseID string) {
	p.responsesClient = newGrokResponsesClient(creds)
	p.responsesClient.LastResponseID = lastResponseID
}

func GrokCredsForTest(p *GrokProvider) *credentials.GrokCredentials { return p.creds }

func CopilotProviderForTest(creds *credentials.CopilotCredentials, apiBaseURL, sessionToken string, expiry time.Time) *CopilotProvider {
	return &CopilotProvider{creds: creds, apiBaseURL: apiBaseURL, sessionToken: sessionToken, sessionTokenExpiry: expiry}
}

func (r *openCodeGoTestRecorder) SetCatalogFailureForTest(fail bool) { r.setCatalogFailure(fail) }

func (r *openCodeGoTestRecorder) LastRequestForPathForTest(t *testing.T, path string) openCodeGoRecordedRequest {
	return r.lastRequestForPath(t, path)
}

func PrimeOpenCodeGoCatalogScopeForTest(apiKey, baseURL string) {
	primeOpenCodeGoCatalogScope(apiKey, baseURL)
}

const ChatGPTOriginatorForTest = chatGPTOriginator

func ChatGPTUserAgentForTest() string { return chatGPTUserAgent() }

const ZenDisplayNameForTest = zenDisplayName

// NewOpenAIProviderForTest builds an OpenAIProvider with private fields set.
func NewOpenAIProviderRawForTest(apiKey, model, effort string, client *ResponsesClient, opts ResponsesOptions) *OpenAIProvider {
	return &OpenAIProvider{apiKey: apiKey, model: model, effort: effort, responsesClient: client, responsesOptions: opts}
}

// NewOpenAIProviderForTest builds an OpenAI provider around a scripted
// Responses client.
func NewOpenAIProviderForTest(apiKey, model string, client *ResponsesClient) *OpenAIProvider {
	return &OpenAIProvider{apiKey: apiKey, model: model, responsesClient: client}
}

func SetOpenAIModelAndResponsesOptionsForTest(p *OpenAIProvider, model string, opts ResponsesOptions) {
	p.model = model
	p.responsesOptions = opts
}

func ChatGPTUseWebSocketForTest(p *ChatGPTProvider) bool { return p.useWebSocket }

func SetChatGPTUseWebSocketForTest(p *ChatGPTProvider, useWebSocket bool) {
	p.useWebSocket = useWebSocket
}
