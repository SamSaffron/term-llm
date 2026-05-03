package llm

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func transcribeAndTruncate(ctx context.Context, filePath string, opts TranscribeOptions) (string, error) {
	transcript, err := TranscribeFile(ctx, filePath, opts)
	if err != nil {
		return "", err
	}
	return TruncateTranscriptIfImplausible(ctx, filePath, transcript), nil
}

// TranscribeWithConfig transcribes an audio file using the provider configured in cfg.
// providerOverride, if non-empty, overrides cfg.Transcription.Provider.
//
// Supported provider names: "openai" (default), "mistral" (Voxtral), "venice", "elevenlabs", "local" (whisper.cpp server),
// "whisper-cli" (whisper.cpp CLI binary). The whisper-cli case is delegated back to cmd via
// the transcribeWhisperCLI function — callers that don't support it (e.g. Telegram) will get
// an unsupported-provider error, which is intentional.
func TranscribeWithConfig(ctx context.Context, cfg *config.Config, filePath, language, providerOverride string) (string, error) {
	providerName := providerOverride
	if providerName == "" {
		providerName = cfg.Transcription.Provider
	}
	if providerName == "" {
		providerName = "openai"
	}

	modelOverride := cfg.Transcription.Model

	switch providerName {
	case "local":
		// whisper.cpp HTTP server — OpenAI-compatible endpoint, typically no auth required
		endpoint := "http://localhost:8080/inference"
		if providerCfg, ok := cfg.Providers["local_whisper"]; ok {
			baseURL := providerCfg.ResolvedURL
			if baseURL == "" {
				baseURL = providerCfg.BaseURL
			}
			if baseURL != "" {
				endpoint = strings.TrimRight(baseURL, "/") + "/inference"
			}
		}
		return transcribeAndTruncate(ctx, filePath, TranscribeOptions{
			Endpoint: endpoint,
			Model:    modelOverride,
			Language: language,
		})

	case "mistral":
		mistralCfg := cfg.Providers["mistral"]
		apiKey := mistralCfg.ResolvedAPIKey
		if apiKey == "" {
			apiKey = os.Getenv("MISTRAL_API_KEY")
		}
		if apiKey == "" {
			return "", fmt.Errorf("transcription provider %q has no API key configured (providers.mistral.api_key or MISTRAL_API_KEY)", providerName)
		}
		endpoint := "https://api.mistral.ai/v1/audio/transcriptions"
		baseURL := mistralCfg.ResolvedURL
		if baseURL == "" {
			baseURL = mistralCfg.BaseURL
		}
		if baseURL != "" {
			endpoint = strings.TrimRight(baseURL, "/") + "/audio/transcriptions"
		}
		model := modelOverride
		if model == "" {
			model = "voxtral-mini-latest"
		}
		return transcribeAndTruncate(ctx, filePath, TranscribeOptions{
			APIKey:   apiKey,
			Endpoint: endpoint,
			Model:    model,
			Language: language,
		})

	case "venice":
		apiKey := cfg.Transcription.Venice.APIKey
		if apiKey == "" {
			apiKey = cfg.Audio.Venice.APIKey
		}
		if apiKey == "" {
			apiKey = cfg.Image.Venice.APIKey
		}
		if apiKey == "" {
			if p, ok := cfg.Providers["venice"]; ok {
				apiKey = p.ResolvedAPIKey
			}
		}
		if apiKey == "" {
			apiKey = os.Getenv("VENICE_API_KEY")
		}
		if apiKey == "" {
			return "", fmt.Errorf("transcription provider %q has no API key configured (transcription.venice.api_key, VENICE_API_KEY, audio.venice.api_key, image.venice.api_key, or providers.venice.api_key)", providerName)
		}
		endpoint := "https://api.venice.ai/api/v1/audio/transcriptions"
		if p, ok := cfg.Providers["venice"]; ok {
			baseURL := p.ResolvedURL
			if baseURL == "" {
				baseURL = p.BaseURL
			}
			if baseURL != "" {
				endpoint = strings.TrimRight(baseURL, "/") + "/audio/transcriptions"
			}
		}
		model := firstNonEmptyString(modelOverride, cfg.Transcription.Venice.Model, "nvidia/parakeet-tdt-0.6b-v3")
		return transcribeAndTruncate(ctx, filePath, TranscribeOptions{
			APIKey:     apiKey,
			Endpoint:   endpoint,
			Model:      model,
			Language:   language,
			Provider:   "venice",
			Timestamps: cfg.Transcription.Timestamps,
		})

	case "elevenlabs":
		apiKey := cfg.Transcription.ElevenLabs.APIKey
		if apiKey == "" {
			apiKey = cfg.Audio.ElevenLabs.APIKey
		}
		if apiKey == "" {
			if p, ok := cfg.Providers["elevenlabs"]; ok {
				apiKey = p.ResolvedAPIKey
			}
		}
		if apiKey == "" {
			apiKey = os.Getenv("ELEVENLABS_API_KEY")
		}
		if apiKey == "" {
			apiKey = os.Getenv("XI_API_KEY")
		}
		if apiKey == "" {
			return "", fmt.Errorf("transcription provider %q has no API key configured (transcription.elevenlabs.api_key, ELEVENLABS_API_KEY, XI_API_KEY, audio.elevenlabs.api_key, or providers.elevenlabs.api_key)", providerName)
		}
		endpoint := "https://api.elevenlabs.io/v1/speech-to-text"
		if p, ok := cfg.Providers["elevenlabs"]; ok {
			baseURL := p.ResolvedURL
			if baseURL == "" {
				baseURL = p.BaseURL
			}
			if baseURL != "" {
				endpoint = strings.TrimRight(baseURL, "/") + "/speech-to-text"
			}
		}
		model := firstNonEmptyString(modelOverride, cfg.Transcription.ElevenLabs.Model, "scribe_v2")
		return transcribeAndTruncate(ctx, filePath, TranscribeOptions{
			APIKey:   apiKey,
			Endpoint: endpoint,
			Model:    model,
			Language: language,
			Provider: "elevenlabs",
		})

	case "openai":
		// Named provider entry takes precedence over env var
		if p, ok := cfg.Providers[string(config.ProviderTypeOpenAI)]; ok && p.ResolvedAPIKey != "" {
			endpoint := ""
			baseURL := p.ResolvedURL
			if baseURL == "" {
				baseURL = p.BaseURL
			}
			if baseURL != "" {
				endpoint = strings.TrimRight(baseURL, "/") + "/audio/transcriptions"
			}
			return transcribeAndTruncate(ctx, filePath, TranscribeOptions{
				APIKey:   p.ResolvedAPIKey,
				Endpoint: endpoint,
				Model:    modelOverride,
				Language: language,
			})
		}
		// Fall back to environment variable
		apiKey := os.Getenv("OPENAI_API_KEY")
		if apiKey == "" {
			return "", fmt.Errorf("transcription is not configured — set providers.openai.api_key or configure a transcription provider in config")
		}
		return transcribeAndTruncate(ctx, filePath, TranscribeOptions{
			APIKey:   apiKey,
			Model:    modelOverride,
			Language: language,
		})

	default:
		// Try looking up by name in providers map (allows any openai_compatible provider)
		if p, ok := cfg.Providers[providerName]; ok {
			apiKey := p.ResolvedAPIKey
			if apiKey == "" {
				return "", fmt.Errorf("transcription provider %q has no API key configured", providerName)
			}
			endpoint := ""
			resolvedURL := p.ResolvedURL
			if resolvedURL != "" {
				endpoint = strings.TrimRight(resolvedURL, "/") + "/audio/transcriptions"
			} else if p.BaseURL != "" {
				endpoint = strings.TrimRight(p.BaseURL, "/") + "/audio/transcriptions"
			}
			return transcribeAndTruncate(ctx, filePath, TranscribeOptions{
				APIKey:   apiKey,
				Endpoint: endpoint,
				Model:    modelOverride,
				Language: language,
			})
		}
		return "", fmt.Errorf("transcription provider %q not found in providers config (supported builtins: openai, mistral, venice, elevenlabs, local, whisper-cli)", providerName)
	}
}
