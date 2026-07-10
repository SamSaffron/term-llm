package llm

import (
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

func TestNewProviderByNameGrokBinNeedsNoAPIKey(t *testing.T) {
	provider, err := NewProviderByName(&config.Config{Providers: map[string]config.ProviderConfig{}}, "grok-bin", "grok-4.5")
	if err != nil {
		t.Fatalf("NewProviderByName: %v", err)
	}
	if provider.Credential() != "grok-bin" {
		t.Fatalf("credential = %q, want grok-bin", provider.Credential())
	}
}

func TestCreateProviderFromConfig_OpenAICompatRequiresProviderName(t *testing.T) {
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
	parseReasoning, includeReasoning, thinkingParam := openAICompatReasoningParserOptions(&config.ProviderConfig{
		Type:    config.ProviderTypeOpenAICompat,
		BaseURL: "https://example.invalid/v1",
	})
	if parseReasoning != nil || includeReasoning != nil || thinkingParam != "" {
		t.Fatalf("reasoning options = %v/%v/%q, want nil/nil/empty", parseReasoning, includeReasoning, thinkingParam)
	}
}

func TestOpenAICompatReasoningParserOptionsReadsExplicitConfig(t *testing.T) {
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
