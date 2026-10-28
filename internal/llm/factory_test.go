package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/factory"
	"github.com/samsaffron/term-llm/internal/llm/providers/anthropic"
	"github.com/samsaffron/term-llm/internal/llm/providers/clibin"
	"github.com/samsaffron/term-llm/internal/llm/providers/ollama"
	"github.com/samsaffron/term-llm/internal/llm/providers/opencompat"
	rp "github.com/samsaffron/term-llm/internal/llm/providers/responsesproviders"
	"github.com/samsaffron/term-llm/internal/providerspec"
)

func TestNewProviderRejectsDisabledProvider(t *testing.T) {
	disabled := false
	cfg := &config.Config{
		DefaultProvider: "xai",
		Providers:       map[string]config.ProviderConfig{"xai": {Enabled: &disabled}},
	}
	t.Setenv("XAI_API_KEY", "xai-test") // credentials alone must not override enabled: false

	if _, err := NewProviderByName(cfg, "xai", ""); err == nil || !strings.Contains(err.Error(), "providers.xai.enabled: false") {
		t.Fatalf("NewProviderByName error = %v, want disabled error", err)
	}
	if _, err := NewProvider(cfg); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("NewProvider error = %v, want disabled error", err)
	}
}

func TestNewProviderByNameFollowsProviderDiscovery(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test")
	t.Setenv("ZEN_API_KEY", "zen-test")

	for _, tc := range []struct {
		mode, name, wantErr string
	}{
		{config.ProviderDiscoveryAuto, "xai", ""},
		{config.ProviderDiscoveryEnv, "xai", ""},
		{config.ProviderDiscoveryConfig, "xai", `provider "xai" is not enabled: provider_discovery is "config"`},
		{config.ProviderDiscoveryConfig, "zen", ""},
		{config.ProviderDiscoveryConfig, "debug", ""},
		{config.ProviderDiscoveryEnv, "grok-bin", `provider "grok-bin" is not enabled: provider_discovery is "env"`},
	} {
		cfg := &config.Config{
			DefaultProvider:   tc.name, // a selector only; it must not enable the provider
			ProviderDiscovery: tc.mode,
			Providers:         map[string]config.ProviderConfig{"zen": {}},
		}
		_, err := NewProviderByName(cfg, tc.name, "")
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s/%s: unexpected error %v", tc.mode, tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s/%s: error = %v, want containing %q", tc.mode, tc.name, err, tc.wantErr)
		}
	}

	// A -p provider:model override must not enable the provider either.
	cfg := &config.Config{ProviderDiscovery: config.ProviderDiscoveryConfig, Providers: map[string]config.ProviderConfig{}}
	cfg.ApplyOverrides("xai", "grok-4")
	if _, err := NewProvider(cfg); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("NewProvider after override error = %v, want not enabled", err)
	}
	if _, err := NewProviderByNameNoRetry(cfg, "xai", ""); err == nil {
		t.Fatal("NewProviderByNameNoRetry bypassed provider_discovery")
	}
}

func TestGetProviderCompletionsFollowsProviderDiscovery(t *testing.T) {
	clearProviderEnv(t)
	cfg := &config.Config{
		ProviderDiscovery: config.ProviderDiscoveryConfig,
		Providers: map[string]config.ProviderConfig{
			"anthropic": {},
			"openai":    {Model: "gpt-5", FromDefaults: true},
		},
	}
	names := GetProviderCompletions("", false, cfg)
	if !slices.Contains(names, "anthropic") || slices.Contains(names, "openai") || slices.Contains(names, "chatgpt") {
		t.Fatalf("provider completions = %v, want only enabled providers", names)
	}
	if got := GetProviderCompletions("openai:", false, cfg); len(got) != 0 {
		t.Fatalf("model completions for a provider not enabled = %v, want none", got)
	}
	if got := GetProviderCompletions("anthropic:", false, cfg); len(got) == 0 {
		t.Fatal("model completions for an enabled provider are empty")
	}

	cfg.ProviderDiscovery = config.ProviderDiscoveryAuto
	if names := GetProviderCompletions("", false, cfg); !slices.Contains(names, "chatgpt") {
		t.Fatalf("auto completions = %v, want every built-in", names)
	}
}

func TestCreateProviderFromConfigKeyProviderEnvFallback(t *testing.T) {
	cases := []struct {
		name   string
		kind   config.ProviderType
		envKey string
		want   core.Provider
	}{
		{"zen", config.ProviderTypeZen, "ZEN_API_KEY", &rp.ZenProvider{}},
		{"venice", config.ProviderTypeVenice, "VENICE_API_KEY", &opencompat.VeniceProvider{}},
		{"nearai", config.ProviderTypeNearAI, "NEARAI_API_KEY", &opencompat.NearAIProvider{}},
		{"sambanova", config.ProviderTypeSambaNova, "SAMBANOVA_API_KEY", &opencompat.SambaNovaProvider{}},
		{"obscurify", providerspec.TypeObscurify, "OBSCURIFY_API_KEY", &opencompat.ObscurifyProvider{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.envKey, "  env-key  ")
			provider, err := factory.Create(tc.name, &config.ProviderConfig{Type: tc.kind}, factoryOptions())
			if err != nil {
				t.Fatal(err)
			}
			if reflect.TypeOf(provider) != reflect.TypeOf(tc.want) {
				t.Fatalf("provider type = %T, want %T", provider, tc.want)
			}
		})
	}
}

func TestCreateProviderFromConfigClaudeBinPreservesHooks(t *testing.T) {
	provider, err := factory.Create("claude-bin", &config.ProviderConfig{
		Type: config.ProviderTypeClaudeBin, EnableHooks: true,
	}, factoryOptions())
	if err != nil {
		t.Fatal(err)
	}
	claude, ok := provider.(*clibin.ClaudeBinProvider)
	if !ok || !claude.HooksEnabled() {
		t.Fatalf("provider = %T, want ClaudeBinProvider with hooks enabled", provider)
	}
}

func TestCreateProviderFromConfigRejectsInvalidOllamaThinkLevel(t *testing.T) {
	_, err := factory.Create("local", &config.ProviderConfig{
		Type:       config.ProviderTypeOllama,
		Model:      "qwen3:14b",
		ThinkLevel: "extreme",
	}, factoryOptions())
	if err == nil || !strings.Contains(err.Error(), "invalid Ollama think_level") {
		t.Fatalf("error = %v, want invalid think_level guidance", err)
	}
}

func TestCreateProviderFromConfigGrokBin(t *testing.T) {
	provider, err := factory.Create("grok-bin", &config.ProviderConfig{
		Type:  config.ProviderTypeGrokBin,
		Model: "grok-4.5-high",
		Env:   map[string]string{"GROK_AUTH_PATH": "/custom/auth.json"},
	}, factoryOptions())
	if err != nil {
		t.Fatalf("createProviderFromConfig: %v", err)
	}
	grok, ok := provider.(*clibin.GrokBinProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *GrokBinProvider", provider)
	}
	model, effort := grok.ConfiguredModel()
	if model != "grok-4.5" || effort != "high" {
		t.Fatalf("provider model/effort = %q/%q", model, effort)
	}
}

func TestCreateProviderFromConfigCursorBin(t *testing.T) {
	provider, err := factory.Create("cursor-bin", &config.ProviderConfig{
		Type:  config.ProviderTypeCursorBin,
		Model: "grok-4.5-high-fast",
		Env:   map[string]string{"CURSOR_API_ENDPOINT": "https://example.test"},
	}, factoryOptions())
	if err != nil {
		t.Fatalf("createProviderFromConfig: %v", err)
	}
	cursor, ok := provider.(*clibin.CursorBinProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *CursorBinProvider", provider)
	}
	model, effort, fast := cursor.ConfiguredModel()
	if model != "grok-4.5" || effort != "high" || !fast {
		t.Fatalf("provider model/effort/fast = %q/%q/%v", model, effort, fast)
	}
}

func TestNewProviderByNameGrokBinNeedsNoAPIKey(t *testing.T) {
	provider, err := NewProviderByName(&config.Config{Providers: map[string]config.ProviderConfig{}}, "grok-bin", "grok-4.5")
	if err != nil {
		t.Fatalf("NewProviderByName: %v", err)
	}
	if provider.Credential() != "grok-bin" {
		t.Fatalf("credential = %q, want grok-bin", provider.Credential())
	}
}

func TestNewProviderByNameCursorBinNeedsNoAPIKey(t *testing.T) {
	provider, err := NewProviderByName(&config.Config{Providers: map[string]config.ProviderConfig{}}, "cursor-bin", "grok-4.5-low")
	if err != nil {
		t.Fatalf("NewProviderByName: %v", err)
	}
	if provider.Credential() != "cursor-bin" {
		t.Fatalf("credential = %q, want cursor-bin", provider.Credential())
	}
}

func TestCreateProviderFromConfigAnthropicCustomBaseURL(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-custom-key")

	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if got := r.Header.Get("X-Api-Key"); got != "sk-custom-key" {
			t.Errorf("X-Api-Key = %q, want %q", got, "sk-custom-key")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[],"has_more":false}`)
	}))
	defer ts.Close()

	provider, err := factory.Create("custom-anthropic", &config.ProviderConfig{
		Type:    config.ProviderTypeAnthropic,
		BaseURL: ts.URL + "/proxy",
		Model:   "custom-model",
	}, factoryOptions())
	if err != nil {
		t.Fatalf("createProviderFromConfig: %v", err)
	}
	anthropicProvider, ok := provider.(*anthropic.AnthropicProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *anthropic.AnthropicProvider", provider)
	}
	if _, err := anthropicProvider.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/proxy/v1/models" {
		t.Fatalf("request path = %q, want %q", gotPath, "/proxy/v1/models")
	}
}

func TestCreateProviderFromConfigRejectsInvalidAnthropicBaseURL(t *testing.T) {
	_, err := factory.Create("custom-anthropic", &config.ProviderConfig{
		Type:           config.ProviderTypeAnthropic,
		BaseURL:        "gateway.example.test/anthropic",
		ResolvedAPIKey: "sk-custom-key",
	}, factoryOptions())
	if err == nil || !strings.Contains(err.Error(), "must include scheme and host") {
		t.Fatalf("error = %v, want invalid base_url guidance", err)
	}
}

func TestCreateProviderFromConfigRoutesLazyBaseURLs(t *testing.T) {
	t.Run("openai compatible appends chat path", func(t *testing.T) {
		var gotPath string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[]}`)
		}))
		defer ts.Close()

		cfg := &config.Config{Providers: map[string]config.ProviderConfig{
			"custom": {
				Type:    config.ProviderTypeOpenAICompat,
				BaseURL: fmt.Sprintf("$(printf %s)", ts.URL+"/v1"),
				Model:   "custom-model",
			},
		}}
		if err := cfg.ResolveProviderCredentials("custom"); err != nil {
			t.Fatalf("ResolveProviderCredentials: %v", err)
		}
		providerCfg := cfg.Providers["custom"]
		provider, err := factory.Create("custom", &providerCfg, factoryOptions())
		if err != nil {
			t.Fatalf("createProviderFromConfig: %v", err)
		}
		compat, ok := provider.(*opencompat.OpenAICompatProvider)
		if !ok {
			t.Fatalf("provider type = %T, want *OpenAICompatProvider", provider)
		}
		resp, err := compat.MakeChatRequest(context.Background(), opencompat.OAIChatRequest{Model: "custom-model"})
		if err != nil {
			t.Fatalf("makeChatRequest: %v", err)
		}
		resp.Body.Close()
		if gotPath != "/v1/chat/completions" {
			t.Fatalf("request path = %q, want %q", gotPath, "/v1/chat/completions")
		}
	})

	t.Run("ollama receives resolved base", func(t *testing.T) {
		cfg := &config.Config{Providers: map[string]config.ProviderConfig{
			"ollama": {
				BaseURL: "$(printf https://ollama.example.test)",
				Model:   "custom-model",
			},
		}}
		if err := cfg.ResolveProviderCredentials("ollama"); err != nil {
			t.Fatalf("ResolveProviderCredentials: %v", err)
		}
		providerCfg := cfg.Providers["ollama"]
		provider, err := factory.Create("ollama", &providerCfg, factoryOptions())
		if err != nil {
			t.Fatalf("createProviderFromConfig: %v", err)
		}
		ollamaProvider, ok := provider.(*ollama.Provider)
		if !ok {
			t.Fatalf("provider type = %T, want *ollama.Provider", provider)
		}
		if ollamaProvider.BaseURL() != "https://ollama.example.test" {
			t.Fatalf("baseURL = %q, want resolved URL", ollamaProvider.BaseURL())
		}
	})
}

func TestCreateProviderFromConfigOpenCodeGo(t *testing.T) {
	provider, err := factory.Create("opencode-go", &config.ProviderConfig{
		Type:           config.ProviderTypeOpenCodeGo,
		ResolvedAPIKey: "test-key",
		Model:          "glm-5.2",
	}, factoryOptions())
	if err != nil {
		t.Fatalf("createProviderFromConfig: %v", err)
	}
	if _, ok := provider.(*rp.OpenCodeGoProvider); !ok {
		t.Fatalf("provider type = %T, want *OpenCodeGoProvider", provider)
	}
}

func TestCreateProviderFromConfigOpenCodeGoUsesBaseURLOverride(t *testing.T) {
	provider, err := factory.Create("opencode-go", &config.ProviderConfig{
		Type:           config.ProviderTypeOpenCodeGo,
		ResolvedAPIKey: "test-key",
		Model:          "muse-spark-1.2-contributor",
		BaseURL:        "https://example.test/muse/v1/",
	}, factoryOptions())
	if err != nil {
		t.Fatalf("createProviderFromConfig: %v", err)
	}
	openCodeGo, ok := provider.(*rp.OpenCodeGoProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *OpenCodeGoProvider", provider)
	}
	if openCodeGo.BaseURL() != "https://example.test/muse/v1" {
		t.Fatalf("baseURL = %q", openCodeGo.BaseURL())
	}
}

func TestCreateProviderFromConfigOpenCodeGoRejectsURLOverride(t *testing.T) {
	_, err := factory.Create("opencode-go", &config.ProviderConfig{
		Type:           config.ProviderTypeOpenCodeGo,
		ResolvedAPIKey: "test-key",
		URL:            "https://example.test/responses",
	}, factoryOptions())
	if err == nil || !strings.Contains(err.Error(), "use base_url") {
		t.Fatalf("error = %v, want base_url guidance", err)
	}
}

func TestCreateProviderFromConfigOpenCodeGoRequiresKey(t *testing.T) {
	_, err := factory.Create("opencode-go", &config.ProviderConfig{Type: config.ProviderTypeOpenCodeGo}, factoryOptions())
	if err == nil || !strings.Contains(err.Error(), "OPENCODE_API_KEY") {
		t.Fatalf("error = %v, want OPENCODE_API_KEY guidance", err)
	}
}

func TestCreateProviderFromConfig_OpenAICompatRequiresProviderName(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("createProviderFromConfig panicked: %v", r)
		}
	}()

	_, err := factory.Create("", &config.ProviderConfig{
		Type:    config.ProviderTypeOpenAICompat,
		BaseURL: "https://example.com/v1",
		Model:   "test-model",
	}, factoryOptions())
	if err == nil {
		t.Fatal("expected empty provider name to return an error")
	}
	if !strings.Contains(err.Error(), "non-empty name") {
		t.Fatalf("expected empty name guidance, got %v", err)
	}
}

func TestOpenAICompatReasoningParserOptionsUsesOnlyExplicitConfig(t *testing.T) {
	t.Parallel()

	parseReasoning, includeReasoning, thinkingParam := factory.OpenAICompatReasoningParserOptions(&config.ProviderConfig{
		Type:    config.ProviderTypeOpenAICompat,
		BaseURL: "https://example.invalid/v1",
	})
	if parseReasoning != nil || includeReasoning != nil || thinkingParam != "" {
		t.Fatalf("reasoning options = %v/%v/%q, want nil/nil/empty", parseReasoning, includeReasoning, thinkingParam)
	}
}

func TestOpenAICompatReasoningParserOptionsReadsExplicitConfig(t *testing.T) {
	t.Parallel()

	no := false
	parseReasoning, includeReasoning, thinkingParam := factory.OpenAICompatReasoningParserOptions(&config.ProviderConfig{
		Type:             config.ProviderTypeOpenAICompat,
		BaseURL:          "https://example.invalid/v1",
		ParseReasoning:   &no,
		IncludeReasoning: &no,
		ThinkingParam:    "custom_thinking",
	})
	if parseReasoning == nil || *parseReasoning {
		t.Fatalf("parseReasoning = %v, want false", parseReasoning)
	}
	if includeReasoning == nil || *includeReasoning {
		t.Fatalf("includeReasoning = %v, want false", includeReasoning)
	}
	if thinkingParam != "custom_thinking" {
		t.Fatalf("thinkingParam = %q, want custom_thinking", thinkingParam)
	}
}

func TestNewProviderByNameNoRetryReturnsUnderlyingAdapter(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{}}
	provider, err := NewProviderByNameNoRetry(cfg, "ollama", "qwen:7b")
	if err != nil {
		t.Fatalf("NewProviderByNameNoRetry: %v", err)
	}
	if _, ok := provider.(*ollama.Provider); !ok {
		t.Fatalf("provider type = %T, want *ollama.Provider", provider)
	}
	wrapped, err := NewProviderByName(cfg, "ollama", "qwen:7b")
	if err != nil {
		t.Fatalf("NewProviderByName: %v", err)
	}
	if _, ok := wrapped.(*RetryProvider); !ok {
		t.Fatalf("production provider type = %T, want *RetryProvider", wrapped)
	}
}

func TestZenRequiresKey(t *testing.T) {
	t.Setenv("ZEN_API_KEY", "")
	for _, configured := range []bool{false, true} {
		cfg := &config.Config{DefaultProvider: "zen", Providers: map[string]config.ProviderConfig{}}
		if configured {
			cfg.Providers["zen"] = config.ProviderConfig{Type: config.ProviderTypeZen}
		}
		if _, err := NewProviderByName(cfg, "zen", ""); err == nil || !strings.Contains(err.Error(), "ZEN_API_KEY") {
			t.Fatalf("configured=%v: error = %v", configured, err)
		}
		if _, err := NewProvider(cfg); err == nil || !strings.Contains(err.Error(), "ZEN_API_KEY") {
			t.Fatalf("default provider configured=%v: error=%v", configured, err)
		}
	}
	t.Setenv("ZEN_API_KEY", "env-key")
	providerFromEnv, err := NewProviderByNameNoRetry(&config.Config{}, "zen", "")
	if err != nil {
		t.Fatal(err)
	}
	zen := providerFromEnv.(*rp.ZenProvider)
	if zen.APIKey() != "env-key" || zen.Model() != "deepseek-v4-flash" {
		t.Fatal("Zen did not use environment credentials and paid default")
	}

	provider, err := factory.Create("zen", &config.ProviderConfig{Type: config.ProviderTypeZen, ResolvedAPIKey: "test-key", Model: "deepseek-v4-flash"}, factoryOptions())
	if err != nil || provider == nil {
		t.Fatalf("keyed Zen: provider=%v error=%v", provider, err)
	}
}
