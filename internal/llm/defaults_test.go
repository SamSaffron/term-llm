package llm

import (
	"github.com/samsaffron/term-llm/internal/llm/providers/ollama"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/providers/gemini"
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

	if got := NewXAIProvider("", "").Model(); got != config.DefaultProviderModel("xai") {
		t.Fatalf("xai default model = %q, want %q", got, config.DefaultProviderModel("xai"))
	}
	if got := gemini.New("", "").Model(); got != config.DefaultProviderModel("gemini") {
		t.Fatalf("gemini default model = %q, want %q", got, config.DefaultProviderModel("gemini"))
	}
	if got := NewVeniceProvider("", "").Model(); got != config.DefaultProviderModel("venice") {
		t.Fatalf("venice default model = %q, want %q", got, config.DefaultProviderModel("venice"))
	}
	if got := NewNearAIProvider("", "").Model(); got != config.DefaultProviderModel("nearai") {
		t.Fatalf("nearai default model = %q, want %q", got, config.DefaultProviderModel("nearai"))
	}
	if got := NewSambaNovaProvider("", "").Model(); got != config.DefaultProviderModel("sambanova") {
		t.Fatalf("sambanova default model = %q, want %q", got, config.DefaultProviderModel("sambanova"))
	}
	if got := ollama.New("", "", ollama.Options{}).Model(); got != config.DefaultProviderModel("ollama") {
		t.Fatalf("ollama default model = %q, want %q", got, config.DefaultProviderModel("ollama"))
	}
}
