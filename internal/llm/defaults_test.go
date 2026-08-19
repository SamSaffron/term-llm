package llm

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestProviderFastModelsMatchConfigDefaults(t *testing.T) {
	t.Parallel()

	for provider, want := range config.DefaultProviderFastModels() {
		if got := ProviderFastModels[provider]; got != want {
			t.Fatalf("ProviderFastModels[%q] = %q, want %q", provider, got, want)
		}
	}
}

func TestProviderConstructorsUseConfigDefaultModels(t *testing.T) {
	t.Parallel()

	if got := NewXAIProvider("", "").model; got != config.DefaultProviderModel("xai") {
		t.Fatalf("xai default model = %q, want %q", got, config.DefaultProviderModel("xai"))
	}
	if got := NewGeminiProvider("", "").model; got != config.DefaultProviderModel("gemini") {
		t.Fatalf("gemini default model = %q, want %q", got, config.DefaultProviderModel("gemini"))
	}
	if got := NewVeniceProvider("", "").model; got != config.DefaultProviderModel("venice") {
		t.Fatalf("venice default model = %q, want %q", got, config.DefaultProviderModel("venice"))
	}
	if got := NewNearAIProvider("", "").model; got != config.DefaultProviderModel("nearai") {
		t.Fatalf("nearai default model = %q, want %q", got, config.DefaultProviderModel("nearai"))
	}
	if got := NewSambaNovaProvider("", "").model; got != config.DefaultProviderModel("sambanova") {
		t.Fatalf("sambanova default model = %q, want %q", got, config.DefaultProviderModel("sambanova"))
	}
	if got := NewOllamaChatProvider("", "", OllamaOptions{}).model; got != config.DefaultProviderModel("ollama") {
		t.Fatalf("ollama default model = %q, want %q", got, config.DefaultProviderModel("ollama"))
	}
	if chatGPTDefaultModel != config.DefaultProviderModel("chatgpt") {
		t.Fatalf("chatgpt default model = %q, want %q", chatGPTDefaultModel, config.DefaultProviderModel("chatgpt"))
	}
	if copilotDefaultModel != config.DefaultProviderModel("copilot") {
		t.Fatalf("copilot default model = %q, want %q", copilotDefaultModel, config.DefaultProviderModel("copilot"))
	}
}
