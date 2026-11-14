package embedding

import (
	"testing"

	"github.com/term-llm/term-llm/internal/config"
)

func TestEmbeddingDefaultsMatchConfig(t *testing.T) {
	checks := map[string]string{
		"openai": openaiDefaultModel,
		"gemini": geminiDefaultModel,
		"jina":   jinaDefaultModel,
		"voyage": voyageDefaultModel,
		"ollama": ollamaDefaultModel,
	}
	want := map[string]string{
		"openai": config.DefaultEmbedOpenAIModel,
		"gemini": config.DefaultEmbedGeminiModel,
		"jina":   config.DefaultEmbedJinaModel,
		"voyage": config.DefaultEmbedVoyageModel,
		"ollama": config.DefaultEmbedOllamaModel,
	}
	for provider, got := range checks {
		if got != want[provider] {
			t.Fatalf("%s embedding default = %q, want %q", provider, got, want[provider])
		}
	}
}
