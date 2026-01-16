package image

import (
	"context"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// ImageResult contains the generated image and metadata
type ImageResult struct {
	Data     []byte // Image data (PNG/JPEG)
	MimeType string // "image/png", "image/jpeg", etc.
}

// GenerateRequest contains parameters for image generation
type GenerateRequest struct {
	Prompt string
	Debug  bool
}

// InputImage represents a single input image for editing
type InputImage struct {
	Data []byte // Image data
	Path string // Path for MIME type detection
}

// EditRequest contains parameters for image editing
type EditRequest struct {
	Prompt      string
	InputImages []InputImage // Input images for editing (supports multiple for some providers)
	Debug       bool
}

// ImageProvider is the interface for image generation providers
type ImageProvider interface {
	// Name returns the provider name for logging
	Name() string

	// Generate creates a new image from a text prompt
	Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error)

	// Edit modifies an existing image based on a prompt
	Edit(ctx context.Context, req EditRequest) (*ImageResult, error)

	// SupportsEdit returns true if the provider supports image editing
	SupportsEdit() bool

	// SupportsMultiImage returns true if the provider supports multiple input images
	SupportsMultiImage() bool
}

// NewImageProvider creates an image provider based on config
func NewImageProvider(cfg *config.Config, providerOverride string) (ImageProvider, error) {
	providerStr := providerOverride
	if providerStr == "" {
		providerStr = cfg.Image.Provider
	}
	if providerStr == "" {
		providerStr = "gemini" // default
	}

	// Parse provider:model syntax
	provider, model := parseImageProviderModel(providerStr)

	switch provider {
	case "gemini":
		apiKey := cfg.Image.Gemini.APIKey
		if apiKey == "" {
			return nil, fmt.Errorf("GEMINI_API_KEY not configured. Set environment variable or add to image.gemini.api_key in config")
		}
		return NewGeminiProvider(apiKey), nil

	case "openai":
		apiKey := cfg.Image.OpenAI.APIKey
		if apiKey == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY not configured. Set environment variable or add to image.openai.api_key in config")
		}
		return NewOpenAIProvider(apiKey), nil

	case "xai", "grok":
		apiKey := cfg.Image.XAI.APIKey
		if apiKey == "" {
			return nil, fmt.Errorf("XAI_API_KEY not configured. Set environment variable or add to image.xai.api_key in config")
		}
		if model == "" {
			model = cfg.Image.XAI.Model
		}
		return NewXAIProvider(apiKey, model), nil

	case "flux", "bfl":
		apiKey := cfg.Image.Flux.APIKey
		if apiKey == "" {
			return nil, fmt.Errorf("BFL_API_KEY not configured. Set environment variable or add to image.flux.api_key in config")
		}
		return NewFluxProvider(apiKey, model), nil

	case "openrouter":
		apiKey := cfg.Image.OpenRouter.APIKey
		if apiKey == "" {
			return nil, fmt.Errorf("OPENROUTER_API_KEY not configured. Set environment variable or add to image.openrouter.api_key in config")
		}
		if model == "" {
			model = cfg.Image.OpenRouter.Model
		}
		return NewOpenRouterProvider(apiKey, model), nil

	default:
		return nil, fmt.Errorf("unknown image provider: %s (valid: gemini, openai, xai, flux, openrouter)", provider)
	}
}

// parseImageProviderModel parses "provider:model" or just "provider" from a string.
// Returns (provider, model). Model will be empty if not specified.
func parseImageProviderModel(s string) (string, string) {
	parts := strings.SplitN(s, ":", 2)
	provider := parts[0]
	model := ""
	if len(parts) == 2 {
		model = parts[1]
	}
	return provider, model
}
