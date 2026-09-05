package llm

import (
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
)

func TestChatGPTContextPolicy(t *testing.T) {
	defer RegisterChatGPTContextConfig(nil)
	for _, tc := range []struct {
		name                                                string
		model                                               string
		backend, maximum, provider, perModel, reserve, want int
	}{
		{name: "astra shipped", model: "gpt-6-astra", backend: 272000, maximum: 872000, want: 372000},
		{name: "sol shipped", model: "gpt-5.6-sol", backend: 272000, maximum: 872000, want: 372000},
		{name: "account ceiling", model: "gpt-6-astra", backend: 272000, maximum: 300000, want: 300000},
		{name: "provider override", model: "gpt-6-astra", backend: 272000, maximum: 872000, provider: 600000, want: 600000},
		{name: "model beats provider", model: "gpt-6-astra", backend: 272000, maximum: 872000, provider: 600000, perModel: 500000, want: 500000},
		{name: "clamp before reserve", model: "gpt-6-astra", backend: 272000, maximum: 872000, perModel: 1000000, reserve: 20000, want: 852000},
		{name: "unknown default", model: "preview", backend: 200000, want: 200000},
		{name: "unknown cannot grow without ceiling", model: "preview", backend: 200000, perModel: 500000, want: 200000},
		{name: "offline bundled ceiling", model: "gpt-6-astra", perModel: 1000000, want: 872000},
		{name: "legacy 5.4 budget", model: "gpt-5.4", backend: 272000, maximum: 1000000, want: 922000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			RegisterChatGPTContextConfig(map[string]config.ProviderConfig{"work": {Type: config.ProviderTypeChatGPT, ContextWindow: tc.provider, ModelConfigs: []config.ProviderModelConfig{{ID: tc.model, ContextWindow: tc.perModel, MaxOutputTokens: tc.reserve}}}})
			facts := ModelInfo{ID: tc.model, BackendContext: tc.backend, MaxContext: tc.maximum}
			got := resolveChatGPTContext("work", facts)
			if got.InputLimit != tc.want {
				t.Fatalf("got %d want %d", got.InputLimit, tc.want)
			}
			if facts.InputLimit != 0 {
				t.Fatal("mutated backend facts")
			}
		})
	}
	RegisterChatGPTContextConfig(nil)
	if got := resolveChatGPTContext("work", ModelInfo{ID: "gpt-6-astra"}).InputLimit; got != 372000 {
		t.Fatalf("reload did not clear overrides: %d", got)
	}
}

func TestChatGPTContextCacheStoresFactsAcrossConfigReload(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	defer RegisterChatGPTContextConfig(nil)
	facts := (chatGPTModelInfo{Slug: "gpt-6-astra", ContextWindow: 272000, MaxContextWindow: 872000}).toModelInfo()
	if err := saveChatGPTModelsCache(chatGPTModelsCache{ClientVersion: chatGPTModelsClientVersion, Models: []ModelInfo{facts}}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []int{600000, 700000} {
		RegisterChatGPTContextConfig(map[string]config.ProviderConfig{"chatgpt": {ContextWindow: target}})
		models, _, err := CachedChatGPTModels()
		if err != nil {
			t.Fatal(err)
		}
		if models[0].InputLimit != target || models[0].RecommendedContext != 372000 || models[0].MaxContext != 872000 {
			t.Fatalf("resolved model: %+v", models[0])
		}
	}
	cache, err := loadChatGPTModelsCache(chatGPTModelsClientVersion)
	if err != nil {
		t.Fatal(err)
	}
	if cache.Models[0].InputLimit != 272000 || cache.Models[0].ConfiguredContext != 0 {
		t.Fatalf("cache contains policy: %+v", cache.Models[0])
	}
}

func TestChatGPTContextEngineUsesScopedAccountCeiling(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	defer RegisterChatGPTContextConfig(nil)
	RegisterChatGPTContextConfig(map[string]config.ProviderConfig{"work": {Type: config.ProviderTypeChatGPT, ContextWindow: 600000, ModelConfigs: []config.ProviderModelConfig{{ID: "gpt-6-astra", Alias: "astra"}}}})
	if err := saveChatGPTModelsCache(chatGPTModelsCache{AccountID: "test", FetchedAt: time.Now(), ClientVersion: chatGPTModelsClientVersion, Models: []ModelInfo{{ID: "gpt-6-astra", BackendContext: 272000, MaxContext: 400000}}}); err != nil {
		t.Fatal(err)
	}
	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{AccountID: "test", AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()}, "gpt-6-astra")
	engine := NewEngine(provider, NewToolRegistry())
	engine.ConfigureContextManagement(provider, "work", "astra-high", true)
	if got := engine.InputLimit(); got != 400000 {
		t.Fatalf("engine context %d want account maximum 400000", got)
	}
}
