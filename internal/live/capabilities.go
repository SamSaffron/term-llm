package live

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// Capabilities is a credential-free snapshot of the configured live voice
// transport. Hosts can expose it directly as JSON.
type Capabilities struct {
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	Voice       string   `json:"voice"`
	Voices      []string `json:"voices"`
	CanSetVoice bool     `json:"can_set_voice"`
}

// ConfigCapabilities resolves the live settings that apply to a new call.
func ConfigCapabilities(cfg config.LiveConfig) Capabilities {
	provider := strings.TrimSpace(cfg.Provider)
	if provider == config.LiveProviderOpenAI {
		model := strings.TrimSpace(cfg.OpenAI.Model)
		if model == "" {
			model = config.DefaultLiveOpenAIModel
		}
		voice := cfg.OpenAI.ResolvedVoice()
		return Capabilities{
			Provider: provider,
			Model:    model,
			Voice:    voice,
			Voices:   cfg.OpenAI.Voices(),
		}
	}
	if provider == config.LiveProviderGemini {
		model := strings.TrimSpace(cfg.Gemini.Model)
		if model == "" {
			model = config.DefaultLiveGeminiModel
		}
		return Capabilities{
			Provider: provider,
			Model:    model,
			Voice:    cfg.Gemini.ResolvedVoice(),
			Voices:   geminiLiveVoices(),
		}
	}
	if provider == "" {
		provider = config.LiveProviderChatGPT
	}
	model := strings.TrimSpace(cfg.ChatGPT.Model)
	if model == "" {
		model = config.DefaultLiveChatGPTModel
	}
	voice := strings.TrimSpace(cfg.ChatGPT.Voice)
	if voice == "" {
		voice = config.DefaultLiveChatGPTVoice
	}
	return Capabilities{
		Provider:    provider,
		Model:       model,
		Voice:       voice,
		Voices:      config.LiveChatGPTVoices(),
		CanSetVoice: true,
	}
}

// CapabilityContext describes authoritative, credential-free host facts for
// the voice model. It is intended for SessionOptions.Context.
func CapabilityContext(capabilities Capabilities) string {
	voiceControl := "cannot be changed during this call"
	if capabilities.CanSetVoice {
		voiceControl = "can be requested through the execution backend using live_settings; provider restrictions may prevent changes after speech begins"
	}
	voices := strings.Join(capabilities.Voices, ", ")
	if voices == "" {
		voices = "none reported"
	}
	return fmt.Sprintf("Authoritative host context: this is term-llm using live provider %s, model %s, and voice %s. Supported voices: %s. The current-call voice %s.", capabilities.Provider, capabilities.Model, capabilities.Voice, voices, voiceControl)
}
