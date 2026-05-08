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
	getProviderConfig := func(name string) (*config.ProviderConfig, error) {
		providerCfg, err := cfg.GetResolvedProviderConfig(name)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		return providerCfg, nil
	}

	switch providerName {
	case "local":
		// whisper.cpp HTTP server — OpenAI-compatible endpoint, typically no auth required
		endpoint := "http://localhost:8080/inference"
		providerCfg, err := getProviderConfig("local_whisper")
		if err != nil {
			return "", err
		}
		if providerCfg != nil {
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
		mistralCfg, err := getProviderConfig("mistral")
		if err != nil {
			return "", err
		}
		if mistralCfg == nil {
			mistralCfg = &config.ProviderConfig{}
		}
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
		veniceProvider, err := getProviderConfig("venice")
		if err != nil {
			return "", err
		}
		apiKey := cfg.Transcription.Venice.APIKey
		if apiKey == "" {
			apiKey = cfg.Audio.Venice.APIKey
		}
		if apiKey == "" {
			apiKey = cfg.Image.Venice.APIKey
		}
		if apiKey == "" && veniceProvider != nil {
			apiKey = veniceProvider.ResolvedAPIKey
		}
		if apiKey == "" {
			apiKey = os.Getenv("VENICE_API_KEY")
		}
		if apiKey == "" {
			return "", fmt.Errorf("transcription provider %q has no API key configured (transcription.venice.api_key, VENICE_API_KEY, audio.venice.api_key, image.venice.api_key, or providers.venice.api_key)", providerName)
		}
		endpoint := "https://api.venice.ai/api/v1/audio/transcriptions"
		if veniceProvider != nil {
			baseURL := veniceProvider.ResolvedURL
			if baseURL == "" {
				baseURL = veniceProvider.BaseURL
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
		elevenLabsProvider, err := getProviderConfig("elevenlabs")
		if err != nil {
			return "", err
		}
		apiKey := cfg.Transcription.ElevenLabs.APIKey
		if apiKey == "" {
			apiKey = cfg.Audio.ElevenLabs.APIKey
		}
		if apiKey == "" && elevenLabsProvider != nil {
			apiKey = elevenLabsProvider.ResolvedAPIKey
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
		if elevenLabsProvider != nil {
			baseURL := elevenLabsProvider.ResolvedURL
			if baseURL == "" {
				baseURL = elevenLabsProvider.BaseURL
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
		openAIProvider, err := getProviderConfig(string(config.ProviderTypeOpenAI))
		if err != nil {
			return "", err
		}
		if openAIProvider != nil && openAIProvider.ResolvedAPIKey != "" {
			endpoint := ""
			baseURL := openAIProvider.ResolvedURL
			if baseURL == "" {
				baseURL = openAIProvider.BaseURL
			}
			if baseURL != "" {
				endpoint = strings.TrimRight(baseURL, "/") + "/audio/transcriptions"
			}
			return transcribeAndTruncate(ctx, filePath, TranscribeOptions{
				APIKey:   openAIProvider.ResolvedAPIKey,
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
		providerCfg, err := getProviderConfig(providerName)
		if err != nil {
			return "", err
		}
		if providerCfg != nil {
			apiKey := providerCfg.ResolvedAPIKey
			if apiKey == "" {
				return "", fmt.Errorf("transcription provider %q has no API key configured", providerName)
			}
			endpoint := ""
			resolvedURL := providerCfg.ResolvedURL
			if resolvedURL != "" {
				endpoint = strings.TrimRight(resolvedURL, "/") + "/audio/transcriptions"
			} else if providerCfg.BaseURL != "" {
				endpoint = strings.TrimRight(providerCfg.BaseURL, "/") + "/audio/transcriptions"
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
