package llm

import (
	"net"
	"net/http"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/providers/opencompat"
)

// Compatibility exports for the OpenAI-compatible provider family, which now
// lives in providers/opencompat.

type (
	OpenAICompatProvider = opencompat.OpenAICompatProvider
	VeniceProvider       = opencompat.VeniceProvider
	XAIProvider          = opencompat.XAIProvider
	NearAIProvider       = opencompat.NearAIProvider
	SambaNovaProvider    = opencompat.SambaNovaProvider
	VLLMProvider         = opencompat.VLLMProvider

	oaiChatRequest    = opencompat.OAIChatRequest
	oaiChatResponse   = opencompat.OAIChatResponse
	oaiStreamOptions  = opencompat.OAIStreamOptions
	oaiModelsResponse = opencompat.OAIModelsResponse
)

func init() {
	opencompat.ParentHooks = opencompat.Hooks{
		InputLimitForProviderModel: InputLimitForProviderModel,
		InputLimitForModel:         InputLimitForModel,
		PricingForProviderModel:    PricingForProviderModel,
		ClampOutputTokens:          ClampOutputTokens,
		HTTPClient:                 func() *http.Client { return defaultHTTPClient },
	}
}

// newStreamingHTTPClient returns a client with transport-level timeouts only,
// so active streams are never killed by an overall request deadline.
func newStreamingHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 2 * time.Minute
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxIdleConnsPerHost = 100
	return &http.Client{Transport: transport}
}

// defaultHTTPClient is the shared streaming client. http.Client.Timeout is
// intentionally unset: it would abort legitimately long-running streams.
var defaultHTTPClient = newStreamingHTTPClient()

func boolPtr(v bool) *bool { return &v }

func setNonEmptyHeaders(header http.Header, values map[string]string) {
	for key, value := range values {
		if value != "" {
			header.Set(key, value)
		}
	}
}

func NewOpenAICompatProvider(baseURL, apiKey, model, name string) *OpenAICompatProvider {
	return opencompat.NewOpenAICompatProvider(baseURL, apiKey, model, name)
}

func NewOpenAICompatProviderWithHeaders(baseURL, apiKey, model, name string, headers map[string]string) *OpenAICompatProvider {
	return opencompat.NewOpenAICompatProviderWithHeaders(baseURL, apiKey, model, name, headers)
}

func NewOpenAICompatProviderFull(baseURL, chatURL, apiKey, model, name string, headers map[string]string) *OpenAICompatProvider {
	return opencompat.NewOpenAICompatProviderFull(baseURL, chatURL, apiKey, model, name, headers)
}

func NewOpenRouterProvider(apiKey, model, appURL, appTitle string) *OpenAICompatProvider {
	return opencompat.NewOpenRouterProvider(apiKey, model, appURL, appTitle)
}

func NewVeniceProvider(apiKey, model string) *VeniceProvider {
	return opencompat.NewVeniceProvider(apiKey, model)
}

func NewXAIProvider(apiKey, model string) *XAIProvider {
	return opencompat.NewXAIProvider(apiKey, model)
}

func NewNearAIProvider(apiKey, model string) *NearAIProvider {
	return opencompat.NewNearAIProvider(apiKey, model)
}

func NewSambaNovaProvider(apiKey, model string) *SambaNovaProvider {
	return opencompat.NewSambaNovaProvider(apiKey, model)
}

func NewVLLMProviderFull(baseURL, chatURL, apiKey, model, name string) *VLLMProvider {
	return opencompat.NewVLLMProviderFull(baseURL, chatURL, apiKey, model, name)
}

func NewVLLMProvider(baseURL, apiKey, model, name string) *VLLMProvider {
	return opencompat.NewVLLMProvider(baseURL, apiKey, model, name)
}

func GetCachedOpenRouterModels(apiKey string) []string {
	return opencompat.GetCachedOpenRouterModels(apiKey)
}

func GetCachedOpenRouterModelInfos(apiKey string) []ModelInfo {
	return opencompat.GetCachedOpenRouterModelInfos(apiKey)
}

func RefreshOpenRouterCacheSync(apiKey string, models []ModelInfo) {
	opencompat.RefreshOpenRouterCacheSync(apiKey, models)
}

func FilterOpenRouterModels(models []string, prefix string) []string {
	return opencompat.FilterOpenRouterModels(models, prefix)
}

func GetCachedVeniceModels(apiKey string) []string { return opencompat.GetCachedVeniceModels(apiKey) }

func GetCachedVeniceModelInfos(apiKey string) []ModelInfo {
	return opencompat.GetCachedVeniceModelInfos(apiKey)
}

func RefreshVeniceCacheSync(models []ModelInfo) { opencompat.RefreshVeniceCacheSync(models) }

func GetVeniceImageModelIDs(cfg *config.Config, modelType string) []string {
	return opencompat.GetVeniceImageModelIDs(cfg, modelType)
}

func openRouterCachedInputLimit(model string) int {
	return opencompat.OpenRouterCachedInputLimit(model)
}

func veniceCachedInputLimit(model string) int { return opencompat.VeniceCachedInputLimit(model) }

func modelInfosFromCache(cached *cache.ModelCache) []ModelInfo {
	return opencompat.ModelInfosFromCache(cached)
}

func modelInfosToCache(models []ModelInfo) []cache.CachedModel {
	return opencompat.ModelInfosToCache(models)
}

func buildCompatMessages(messages []Message) []opencompat.OAIMessage {
	return opencompat.BuildCompatMessages(messages)
}

func buildCompatTools(specs []ToolSpec) ([]opencompat.OAITool, error) {
	return opencompat.BuildCompatTools(specs)
}

func buildCompatToolChoice(choice ToolChoice) interface{} {
	return opencompat.BuildCompatToolChoice(choice)
}

func newCompatToolState() *opencompat.CompatToolState { return opencompat.NewCompatToolState() }
