package llm

import (
	"net/http"

	"context"
	"github.com/samsaffron/term-llm/internal/config"

	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/llm/providers/opencompat"
	rp "github.com/samsaffron/term-llm/internal/llm/providers/responsesproviders"
)

// The Responses-dependent providers (OpenAI, ChatGPT, Copilot, Grok, OpenCode
// Zen and OpenCode Go) live in providers/responsesproviders. These aliases
// and wrappers keep the original llm exports and the unexported names used by
// the remaining root files.

func init() {
	rp.ParentHooks = rp.Hooks{
		StaticModelEntries: func(providerType string) []rp.StaticModelEntry {
			entries := ProviderModels[providerType]
			out := make([]rp.StaticModelEntry, 0, len(entries))
			for _, entry := range entries {
				out = append(out, rp.StaticModelEntry{
					ID:               entry.ID,
					InputLimit:       entry.InputLimit,
					OutputLimit:      entry.OutputLimit,
					ReasoningEfforts: entry.ReasoningEfforts,
				})
			}
			return out
		},
		BaseModelAndEffortForProvider:       BaseModelAndEffortForProvider,
		ConfigBaseModelAndEffortForProvider: configBaseModelAndEffortForProvider,
	}
}

func cloneFileUploadPolicy(policy *FileUploadPolicy) *FileUploadPolicy {
	if policy == nil {
		return nil
	}
	clone := *policy
	clone.NativeMimeTypes = cloneStrings(policy.NativeMimeTypes)
	clone.TextEmbedMimeTypes = cloneStrings(policy.TextEmbedMimeTypes)
	return &clone
}

// ParseModelEffort extracts effort suffix from model name.
func ParseModelEffort(model string) (string, string) { return opencompat.ParseModelEffort(model) }

type (
	ChatGPTProvider        = rp.ChatGPTProvider
	ChatGPTProviderOptions = rp.ChatGPTProviderOptions
	CopilotProvider        = rp.CopilotProvider
	GrokProvider           = rp.GrokProvider
	GrokProviderOptions    = rp.GrokProviderOptions
	OpenAIProvider         = rp.OpenAIProvider
	OpenAIProviderOptions  = rp.OpenAIProviderOptions
	OpenCodeGoProvider     = rp.OpenCodeGoProvider
	ZenProvider            = rp.ZenProvider
)

func CachedChatGPTModels() (models []ModelInfo, fresh bool, err error) {
	return rp.CachedChatGPTModels()
}

func CachedGrokModels() ([]ModelInfo, bool, error) { return rp.CachedGrokModels() }

func CachedOpenCodeGoModels() ([]ModelInfo, bool, error) { return rp.CachedOpenCodeGoModels() }

func ChatGPTRequestHeaders(creds *credentials.ChatGPTCredentials) map[string]string {
	return rp.ChatGPTRequestHeaders(creds)
}

func ExpandCachedZenReasoningVariants(models []string) []string {
	return rp.ExpandCachedZenReasoningVariants(models)
}

func GetCachedChatGPTModels() []string { return rp.GetCachedChatGPTModels() }

func GetCachedCopilotModelInfos() []ModelInfo { return rp.GetCachedCopilotModelInfos() }

func GetCachedCopilotModels() []string { return rp.GetCachedCopilotModels() }

func GetCachedOpenCodeGoModels() []string { return rp.GetCachedOpenCodeGoModels() }

func GetCachedOpenCodeGoModelsForAPIKey(apiKey string) []string {
	return rp.GetCachedOpenCodeGoModelsForAPIKey(apiKey)
}

func GetCachedZenModelInfos() []ModelInfo { return rp.GetCachedZenModelInfos() }

func GetCachedZenModels() []string { return rp.GetCachedZenModels() }

func LogoutGrok(ctx context.Context) (warning string, err error) { return rp.LogoutGrok(ctx) }

func NewChatGPTProvider(model string) (*ChatGPTProvider, error) { return rp.NewChatGPTProvider(model) }

func NewChatGPTProviderWithCreds(creds *credentials.ChatGPTCredentials, model string) *ChatGPTProvider {
	return rp.NewChatGPTProviderWithCreds(creds, model)
}

func NewChatGPTProviderWithCredsAndOptions(creds *credentials.ChatGPTCredentials, model string, opts ChatGPTProviderOptions) *ChatGPTProvider {
	return rp.NewChatGPTProviderWithCredsAndOptions(creds, model, opts)
}

func NewChatGPTProviderWithOptions(model string, opts ChatGPTProviderOptions) (*ChatGPTProvider, error) {
	return rp.NewChatGPTProviderWithOptions(model, opts)
}

func NewChatGPTResponsesClient(creds *credentials.ChatGPTCredentials) *ResponsesClient {
	return rp.NewChatGPTResponsesClient(creds)
}

func NewCopilotProvider(model string) (*CopilotProvider, error) { return rp.NewCopilotProvider(model) }

func NewCopilotProviderWithCreds(creds *credentials.CopilotCredentials, model string) *CopilotProvider {
	return rp.NewCopilotProviderWithCreds(creds, model)
}

func NewGrokProvider(model string) (*GrokProvider, error) { return rp.NewGrokProvider(model) }

func NewGrokProviderWithCreds(creds *credentials.GrokCredentials, model string) *GrokProvider {
	return rp.NewGrokProviderWithCreds(creds, model)
}

func NewGrokProviderWithCredsAndOptions(creds *credentials.GrokCredentials, model string, opts GrokProviderOptions) *GrokProvider {
	return rp.NewGrokProviderWithCredsAndOptions(creds, model, opts)
}

func NewGrokProviderWithOptions(model string, opts GrokProviderOptions) (*GrokProvider, error) {
	return rp.NewGrokProviderWithOptions(model, opts)
}

func NewOpenAIProvider(apiKey string, model string) *OpenAIProvider {
	return rp.NewOpenAIProvider(apiKey, model)
}

func NewOpenAIProviderWithOptions(apiKey string, model string, opts OpenAIProviderOptions) *OpenAIProvider {
	return rp.NewOpenAIProviderWithOptions(apiKey, model, opts)
}

func NewOpenCodeGoProvider(apiKey string, model string) *OpenCodeGoProvider {
	return rp.NewOpenCodeGoProvider(apiKey, model)
}

func NewOpenCodeGoProviderWithBaseURL(apiKey string, model string, baseURL string) *OpenCodeGoProvider {
	return rp.NewOpenCodeGoProviderWithBaseURL(apiKey, model, baseURL)
}

func NewZenProvider(apiKey string, model string) *ZenProvider {
	return rp.NewZenProvider(apiKey, model)
}

func PromptForChatGPTAuth() (*credentials.ChatGPTCredentials, error) {
	return rp.PromptForChatGPTAuth()
}

func PromptForCopilotAuth() (*credentials.CopilotCredentials, error) {
	return rp.PromptForCopilotAuth()
}

func PromptForGrokAuth() (*credentials.GrokCredentials, error) { return rp.PromptForGrokAuth() }

func RefreshCopilotCacheSync(models []ModelInfo) { rp.RefreshCopilotCacheSync(models) }

func RefreshZenCacheSync(models []ModelInfo) { rp.RefreshZenCacheSync(models) }

func RefreshZenModelsIfStale(ctx context.Context, apiKey string, model string) error {
	return rp.RefreshZenModelsIfStale(ctx, apiKey, model)
}

func RegisterChatGPTContextConfig(providers map[string]config.ProviderConfig) {
	rp.RegisterChatGPTContextConfig(providers)
}

func TranscribeChatGPT(ctx context.Context, filePath string, language string) (string, error) {
	return rp.TranscribeChatGPT(ctx, filePath, language)
}

const (
	chatGPTOriginator             = rp.ChatGPTOriginator
	opencodeGoBaseURL             = rp.OpenCodeGoBaseURL
	opencodeGoRefreshTimeout      = rp.OpenCodeGoRefreshTimeout
	grokProxyCompatibilityVersion = rp.GrokProxyCompatibilityVersion
	grokUserAgent                 = rp.GrokUserAgent
)

var (
	grok46ReasoningEfforts = rp.Grok46ReasoningEfforts()
	chatGPTHTTPClient      = rp.ChatGPTHTTPClient()
)

func chatGPTUserAgent() string { return rp.ChatGPTUserAgent() }

func cachedChatGPTModelFacts() ([]ModelInfo, bool, error) { return rp.CachedChatGPTModelFacts() }

func chatGPTModelServiceTier(model string) (string, string) {
	return rp.ChatGPTModelServiceTier(model)
}

func chatGPTCachedModelInfo(model string) (ModelInfo, bool) {
	return rp.ChatGPTCachedModelInfo(model)
}

func chatGPTContextModelID(provider, model string) string {
	return rp.ChatGPTContextModelID(provider, model)
}

func resolveChatGPTContext(provider string, info ModelInfo) ModelInfo {
	return rp.ResolveChatGPTContext(provider, info)
}

func copilotCachedInputLimit(model string) int { return rp.CopilotCachedInputLimit(model) }

func grokCachedInputLimit(model string) int { return rp.GrokCachedInputLimit(model) }

func grokCachedReasoningEfforts(model string) ([]string, bool) {
	return rp.GrokCachedReasoningEfforts(model)
}

func staticGrokReasoningEfforts(id string) []string { return rp.StaticGrokReasoningEfforts(id) }

func refreshGrokSession(ctx context.Context, creds *credentials.GrokCredentials, force bool) error {
	return rp.RefreshGrokSession(ctx, creds, force)
}

func opencodeGoCachedInputLimit(model string) int { return rp.OpenCodeGoCachedInputLimit(model) }

func opencodeGoCachedReasoningEfforts(model string) []string {
	return rp.OpenCodeGoCachedReasoningEfforts(model)
}

func getCachedOpenCodeGoModelsForScope(apiKey, baseURL string) []string {
	return rp.GetCachedOpenCodeGoModelsForScope(apiKey, baseURL)
}

func primeOpenCodeGoCatalogScope(apiKey, baseURL string) {
	rp.PrimeOpenCodeGoCatalogScope(apiKey, baseURL)
}

func applyOpenCodeGoAttributionHeaders(header http.Header, sessionID string) {
	rp.ApplyOpenCodeGoAttributionHeaders(header, sessionID)
}
