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
	Prompt      string
	Size        string // Normalized resolution: "1K", "2K", "4K"; providers translate or ignore
	AspectRatio string // e.g. "1:1", "16:9", "9:16"; providers translate or ignore
	Debug       bool
	DebugRaw    bool
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
	Size        string       // Normalized resolution: "1K", "2K", "4K"; providers translate or ignore
	AspectRatio string       // e.g. "1:1", "16:9", "9:16"; providers translate or ignore
	Debug       bool
	DebugRaw    bool
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
		if model == "" {
			model = cfg.Image.Gemini.Model
		}
		if err := ValidateSize(cfg.Image.Gemini.ImageSize); err != nil {
			return nil, fmt.Errorf("invalid image.gemini.image_size config: %w", err)
		}
		return NewGeminiProvider(apiKey, model, cfg.Image.Gemini.ImageSize), nil

	case "openai":
		apiKey := cfg.Image.OpenAI.APIKey
		if apiKey == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY not configured. Set environment variable or add to image.openai.api_key in config")
		}
		if model == "" {
			model = cfg.Image.OpenAI.Model
		}
		return NewOpenAIProvider(apiKey, model), nil

	case "chatgpt":
		if model == "" {
			model = cfg.Image.ChatGPT.Model
		}
		return NewChatGPTProvider(model)

	case "xai", "grok":
		apiKey := cfg.Image.XAI.APIKey
		if apiKey == "" {
			return nil, fmt.Errorf("XAI_API_KEY not configured. Set environment variable or add to image.xai.api_key in config")
		}
		if model == "" {
			model = cfg.Image.XAI.Model
		}
		return NewXAIProvider(apiKey, model), nil

	case "venice":
		apiKey := cfg.Image.Venice.APIKey
		if apiKey == "" {
			return nil, fmt.Errorf("VENICE_API_KEY not configured. Set environment variable or add to image.venice.api_key in config")
		}
		if model == "" {
			model = cfg.Image.Venice.Model
		}
		editModel := cfg.Image.Venice.EditModel
		return NewVeniceProvider(apiKey, model, editModel, cfg.Image.Venice.Resolution), nil

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

	case "debug":
		return NewDebugProvider(cfg.Image.Debug.Delay), nil

	default:
		return nil, fmt.Errorf("unknown image provider: %s (valid: debug, gemini, openai, chatgpt, xai, venice, flux, openrouter)", provider)
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

// ValidSizes are the normalized image size values accepted by the system.
var ValidSizes = []string{"1K", "2K", "4K"}

// ValidateSize checks that size is a recognized normalized value.
// Returns nil for empty string (no size requested). Returns an error for invalid values.
func ValidateSize(size string) error {
	if size == "" {
		return nil
	}
	for _, v := range ValidSizes {
		if size == v {
			return nil
		}
	}
	return fmt.Errorf("invalid size %q (valid: %s)", size, strings.Join(ValidSizes, ", "))
}
