package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestCreateProviderFromConfigGrokBin(t *testing.T) {
	provider, err := createProviderFromConfig("grok-bin", &config.ProviderConfig{
		Type:  config.ProviderTypeGrokBin,
		Model: "grok-4.5-high",
		Env:   map[string]string{"GROK_AUTH_PATH": "/custom/auth.json"},
	})
	if err != nil {
		t.Fatalf("createProviderFromConfig: %v", err)
	}
	grok, ok := provider.(*GrokBinProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *GrokBinProvider", provider)
	}
	if grok.model != "grok-4.5" || grok.effort != "high" {
		t.Fatalf("provider model/effort = %q/%q", grok.model, grok.effort)
	}
}

func TestCreateProviderFromConfigCursorBin(t *testing.T) {
	provider, err := createProviderFromConfig("cursor-bin", &config.ProviderConfig{
		Type:  config.ProviderTypeCursorBin,
		Model: "grok-4.5-high-fast",
		Env:   map[string]string{"CURSOR_API_ENDPOINT": "https://example.test"},
	})
	if err != nil {
		t.Fatalf("createProviderFromConfig: %v", err)
	}
	cursor, ok := provider.(*CursorBinProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *CursorBinProvider", provider)
	}
	if cursor.model != "grok-4.5" || cursor.effort != "high" || !cursor.fast {
		t.Fatalf("provider model/effort/fast = %q/%q/%v", cursor.model, cursor.effort, cursor.fast)
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

	provider, err := createProviderFromConfig("custom-anthropic", &config.ProviderConfig{
		Type:    config.ProviderTypeAnthropic,
		BaseURL: ts.URL + "/proxy",
		Model:   "custom-model",
	})
	if err != nil {
		t.Fatalf("createProviderFromConfig: %v", err)
	}
	anthropicProvider, ok := provider.(*AnthropicProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *AnthropicProvider", provider)
	}
	if _, err := anthropicProvider.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/proxy/v1/models" {
		t.Fatalf("request path = %q, want %q", gotPath, "/proxy/v1/models")
	}
}

func TestCreateProviderFromConfigRejectsInvalidAnthropicBaseURL(t *testing.T) {
	_, err := createProviderFromConfig("custom-anthropic", &config.ProviderConfig{
		Type:           config.ProviderTypeAnthropic,
		BaseURL:        "gateway.example.test/anthropic",
		ResolvedAPIKey: "sk-custom-key",
	})
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
		provider, err := createProviderFromConfig("custom", &providerCfg)
		if err != nil {
			t.Fatalf("createProviderFromConfig: %v", err)
		}
		compat, ok := provider.(*OpenAICompatProvider)
		if !ok {
			t.Fatalf("provider type = %T, want *OpenAICompatProvider", provider)
		}
		resp, err := compat.makeChatRequest(context.Background(), oaiChatRequest{Model: "custom-model"})
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
		provider, err := createProviderFromConfig("ollama", &providerCfg)
		if err != nil {
			t.Fatalf("createProviderFromConfig: %v", err)
		}
		ollama, ok := provider.(*OllamaProvider)
		if !ok {
			t.Fatalf("provider type = %T, want *OllamaProvider", provider)
		}
		if ollama.baseURL != "https://ollama.example.test" {
			t.Fatalf("baseURL = %q, want resolved URL", ollama.baseURL)
		}
	})
}

func TestCreateProviderFromConfig_OpenAICompatRequiresProviderName(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("createProviderFromConfig panicked: %v", r)
		}
	}()

	_, err := createProviderFromConfig("", &config.ProviderConfig{
		Type:    config.ProviderTypeOpenAICompat,
		BaseURL: "https://example.com/v1",
		Model:   "test-model",
	})
	if err == nil {
		t.Fatal("expected empty provider name to return an error")
	}
	if !strings.Contains(err.Error(), "non-empty name") {
		t.Fatalf("expected empty name guidance, got %v", err)
	}
}

func TestOpenAICompatReasoningParserOptionsUsesOnlyExplicitConfig(t *testing.T) {
	t.Parallel()

	parseReasoning, includeReasoning, thinkingParam := openAICompatReasoningParserOptions(&config.ProviderConfig{
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
	parseReasoning, includeReasoning, thinkingParam := openAICompatReasoningParserOptions(&config.ProviderConfig{
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
