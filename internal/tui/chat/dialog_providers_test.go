package chat

import (
	"slices"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm"
)

func TestGetAvailableProviders_FiltersProviderAllowedModels(t *testing.T) {
	previous := providerHasLocalCredentials
	providerHasLocalCredentials = func(string) bool { return false }
	t.Cleanup(func() { providerHasLocalCredentials = previous })
	llm.RegisterConfigReasoningEfforts([]llm.ConfigModelReasoningEfforts{
		{Provider: "corp", Model: "upstream/model", Efforts: []string{"low", "high"}},
		{Provider: "corp", Model: "friendly", Efforts: []string{"low", "high"}},
	})
	t.Cleanup(func() { llm.RegisterConfigReasoningEfforts(nil) })

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"corp": {
		Type:          config.ProviderTypeOpenAICompat,
		Model:         "friendly",
		Models:        []string{"friendly", "blocked"},
		AllowedModels: []string{"upstream/model-high"},
		ModelConfigs: []config.ProviderModelConfig{{
			ID: "upstream/model", Alias: "friendly", ReasoningEfforts: []string{"low", "high"},
		}},
	}}}
	providers := GetAvailableProviders(cfg)
	if len(providers) != 1 || providers[0].Name != "corp" {
		t.Fatalf("GetAvailableProviders() = %+v, want only corp", providers)
	}
	if want := []string{"friendly-high"}; !slices.Equal(providers[0].Models, want) {
		t.Fatalf("corp models = %v, want %v", providers[0].Models, want)
	}
}

func TestGetAvailableProviders_ListsOnlyConfiguredProviders(t *testing.T) {
	t.Setenv("XAI_API_KEY", "xai-test")
	previous := providerHasLocalCredentials
	providerHasLocalCredentials = func(name string) bool { return name == "copilot" || name == "claude-bin" }
	t.Cleanup(func() { providerHasLocalCredentials = previous })

	enabled, disabled := true, false
	cfg := &config.Config{
		DefaultProvider: "chatgpt",
		Providers: map[string]config.ProviderConfig{
			"openai":    {Enabled: &enabled},
			"anthropic": {Model: "claude-sonnet-4-6", FromDefaults: true}, // schema default only
			"copilot":   {Enabled: &disabled},                             // signed in, but disabled
			"corp-llm":  {Type: config.ProviderTypeOpenAICompat, Models: []string{"m1"}},
			"old-llm":   {Type: config.ProviderTypeOpenAICompat, Models: []string{"m2"}, Enabled: &disabled},
		},
	}

	var got []string
	for _, p := range GetAvailableProviders(cfg) {
		got = append(got, p.Name)
	}
	slices.Sort(got)
	want := []string{"chatgpt", "claude-bin", "corp-llm", "openai", "xai"}
	if !slices.Equal(got, want) {
		t.Fatalf("GetAvailableProviders = %v, want %v", got, want)
	}
}
