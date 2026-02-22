package embedding

import (
	"fmt"
	"math"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// EmbeddingResult contains the embeddings and metadata from an API call
type EmbeddingResult struct {
	Model      string      `json:"model"`
	Dimensions int         `json:"dimensions"`
	Embeddings []Embedding `json:"embeddings"`
	Usage      *UsageInfo  `json:"usage,omitempty"`
}

// Embedding holds a single text's embedding vector
type Embedding struct {
	Text   string    `json:"text"`
	Index  int       `json:"index"`
	Vector []float64 `json:"vector"`
}

// UsageInfo contains token usage information
type UsageInfo struct {
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// EmbedRequest contains parameters for generating embeddings
type EmbedRequest struct {
	Texts      []string // Input texts to embed
	Model      string   // Model override (empty = provider default)
	Dimensions int      // Custom dimensions (0 = model default)
	TaskType   string   // Gemini task type hint (empty = none)
}

// EmbeddingProvider is the interface for embedding providers
type EmbeddingProvider interface {
	// Name returns the provider name for display
	Name() string

	// DefaultModel returns the default embedding model for this provider
	DefaultModel() string

	// Embed generates embeddings for the given texts
	Embed(req EmbedRequest) (*EmbeddingResult, error)
}

// NewEmbeddingProvider creates an embedding provider based on config
func NewEmbeddingProvider(cfg *config.Config, providerOverride string) (EmbeddingProvider, error) {
	providerStr := strings.TrimSpace(providerOverride)
	if providerStr == "" {
		providerStr = strings.TrimSpace(cfg.Embed.Provider)
	}
	if providerStr == "" {
		providerStr = inferEmbeddingProvider(cfg)
	}
	if providerStr == "" {
		return nil, fmt.Errorf("no embedding provider configured. Set embed.provider or configure GEMINI_API_KEY/OPENAI_API_KEY")
	}

	// Parse provider:model syntax
	provider, model := parseProviderModel(providerStr)

	switch provider {
	case "openai":
		apiKey := cfg.Embed.OpenAI.APIKey
		if apiKey == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY not configured. Set environment variable or add to embed.openai.api_key in config")
		}
		p := NewOpenAIProvider(apiKey)
		if model != "" {
			p.model = model
		}
		return p, nil

	case "gemini":
		apiKey := cfg.Embed.Gemini.APIKey
		if apiKey == "" {
			return nil, fmt.Errorf("GEMINI_API_KEY not configured. Set environment variable or add to embed.gemini.api_key in config")
		}
		p := NewGeminiProvider(apiKey)
		if model != "" {
			p.model = model
		}
		return p, nil

	case "jina":
		apiKey := cfg.Embed.Jina.APIKey
		if apiKey == "" {
			return nil, fmt.Errorf("JINA_API_KEY not configured. Get a free key at https://jina.ai/embeddings/ and set JINA_API_KEY or add to embed.jina.api_key in config")
		}
		p := NewJinaProvider(apiKey)
		if model != "" {
			p.model = model
		}
		return p, nil

	case "voyage":
		apiKey := cfg.Embed.Voyage.APIKey
		if apiKey == "" {
			return nil, fmt.Errorf("VOYAGE_API_KEY not configured. Set environment variable or add to embed.voyage.api_key in config")
		}
		p := NewVoyageProvider(apiKey)
		if model != "" {
			p.model = model
		}
		return p, nil

	case "ollama":
		baseURL := cfg.Embed.Ollama.BaseURL
		if baseURL == "" {
			baseURL = "http://localhost:11434"
		}
		p := NewOllamaProvider(baseURL)
		if model != "" {
			p.model = model
		}
		return p, nil

	default:
		return nil, fmt.Errorf("unknown embedding provider: %s (valid: gemini, openai, jina, voyage, ollama)", provider)
	}
}

// InferEmbeddingProvider returns an available embedding provider based on
// configured credentials. Preference order: gemini, then openai.
func InferEmbeddingProvider(cfg *config.Config) string {
	return inferEmbeddingProvider(cfg)
}

// inferEmbeddingProvider returns the best available provider based on configured
// embedding credentials. Returns empty string when no provider can be inferred.
func inferEmbeddingProvider(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if strings.TrimSpace(cfg.Embed.Gemini.APIKey) != "" {
		return "gemini"
	}
	if strings.TrimSpace(cfg.Embed.OpenAI.APIKey) != "" {
		return "openai"
	}
	return ""
}

// parseProviderModel parses "provider:model" or just "provider" from a string.
func parseProviderModel(s string) (string, string) {
	parts := strings.SplitN(s, ":", 2)
	provider := strings.TrimSpace(parts[0])
	model := ""
	if len(parts) == 2 {
		model = strings.TrimSpace(parts[1])
	}
	return provider, model
}

// CosineSimilarity computes the cosine similarity between two vectors.
// Returns a value between -1 and 1, where 1 means identical direction.
func CosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dotProduct, normA, normB float64
	for i := range a {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dotProduct / denom
}
