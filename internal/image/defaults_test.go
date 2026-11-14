package image

import (
	"testing"

	"github.com/term-llm/term-llm/internal/config"
)

func TestImageDefaultsMatchConfig(t *testing.T) {
	checks := map[string]string{
		"gemini":     geminiDefaultModel,
		"openai":     openaiDefaultModel,
		"chatgpt":    chatGPTImageDefaultModel,
		"xai":        xaiImageModel,
		"venice":     veniceDefaultModel,
		"flux":       fluxDefaultGenerateModel,
		"openrouter": NewOpenRouterProvider("", "").model,
	}
	want := map[string]string{
		"gemini":     config.DefaultImageGeminiModel,
		"openai":     config.DefaultImageOpenAIModel,
		"chatgpt":    config.DefaultImageChatGPTModel,
		"xai":        config.DefaultImageXAIModel,
		"venice":     config.DefaultImageVeniceModel,
		"flux":       config.DefaultImageFluxModel,
		"openrouter": config.DefaultImageOpenRouterModel,
	}
	for provider, got := range checks {
		if got != want[provider] {
			t.Fatalf("%s image default = %q, want %q", provider, got, want[provider])
		}
	}
	if veniceDefaultResolution != config.DefaultImageVeniceResolution {
		t.Fatalf("venice resolution = %q, want %q", veniceDefaultResolution, config.DefaultImageVeniceResolution)
	}
	if fluxDefaultEditModel != config.DefaultImageFluxEditModel {
		t.Fatalf("flux edit model = %q, want %q", fluxDefaultEditModel, config.DefaultImageFluxEditModel)
	}
}
