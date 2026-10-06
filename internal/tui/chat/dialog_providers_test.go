package chat

import (
	"slices"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

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
