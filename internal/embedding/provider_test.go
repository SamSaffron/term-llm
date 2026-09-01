package embedding

import (
	"math"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestNewEmbeddingProviderVeniceModel(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"venice": {APIKey: "venice-key"},
	}}
	provider, err := NewEmbeddingProvider(cfg, "venice:text-embedding-qwen3-8b")
	if err != nil {
		t.Fatalf("NewEmbeddingProvider: %v", err)
	}
	venice, ok := provider.(*VeniceProvider)
	if !ok {
		t.Fatalf("provider type = %T", provider)
	}
	if venice.model != "text-embedding-qwen3-8b" {
		t.Fatalf("model = %q", venice.model)
	}
}

func TestNewEmbeddingProviderChatGPTOAuthModel(t *testing.T) {
	provider, err := NewEmbeddingProvider(&config.Config{}, "chatgpt:text-embedding-3-large")
	if err != nil {
		t.Fatalf("NewEmbeddingProvider: %v", err)
	}
	chatgpt, ok := provider.(*ChatGPTProvider)
	if !ok {
		t.Fatalf("provider type = %T", provider)
	}
	if chatgpt.model != "text-embedding-3-large" {
		t.Fatalf("model = %q", chatgpt.model)
	}
}

func TestNewEmbeddingProviderResolvesOllamaBaseURL(t *testing.T) {
	cfg := &config.Config{}
	cfg.Embed.Ollama.BaseURL = "$(printf https://ollama.example.test)"

	provider, err := NewEmbeddingProvider(cfg, "ollama")
	if err != nil {
		t.Fatalf("NewEmbeddingProvider: %v", err)
	}
	ollama, ok := provider.(*OllamaProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *OllamaProvider", provider)
	}
	if want := "https://ollama.example.test"; ollama.baseURL != want {
		t.Fatalf("baseURL = %q, want %q", ollama.baseURL, want)
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name     string
		a        []float64
		b        []float64
		expected float64
	}{
		{
			name:     "identical vectors",
			a:        []float64{1, 0, 0},
			b:        []float64{1, 0, 0},
			expected: 1.0,
		},
		{
			name:     "opposite vectors",
			a:        []float64{1, 0, 0},
			b:        []float64{-1, 0, 0},
			expected: -1.0,
		},
		{
			name:     "orthogonal vectors",
			a:        []float64{1, 0, 0},
			b:        []float64{0, 1, 0},
			expected: 0.0,
		},
		{
			name:     "similar vectors",
			a:        []float64{1, 1, 0},
			b:        []float64{1, 0, 0},
			expected: 1.0 / math.Sqrt(2),
		},
		{
			name:     "zero vector",
			a:        []float64{0, 0, 0},
			b:        []float64{1, 0, 0},
			expected: 0.0,
		},
		{
			name:     "empty vectors",
			a:        []float64{},
			b:        []float64{},
			expected: 0.0,
		},
		{
			name:     "mismatched lengths",
			a:        []float64{1, 2},
			b:        []float64{1, 2, 3},
			expected: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CosineSimilarity(tt.a, tt.b)
			if math.Abs(result-tt.expected) > 1e-10 {
				t.Errorf("CosineSimilarity(%v, %v) = %v, want %v", tt.a, tt.b, result, tt.expected)
			}
		})
	}
}

func TestParseProviderModel(t *testing.T) {
	tests := []struct {
		input    string
		provider string
		model    string
	}{
		{"openai", "openai", ""},
		{"openai:text-embedding-3-large", "openai", "text-embedding-3-large"},
		{"chatgpt:text-embedding-3-large", "chatgpt", "text-embedding-3-large"},
		{"venice:text-embedding-qwen3-8b", "venice", "text-embedding-qwen3-8b"},
		{"gemini", "gemini", ""},
		{"gemini:gemini-embedding-001", "gemini", "gemini-embedding-001"},
		{"ollama:nomic-embed-text", "ollama", "nomic-embed-text"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			p, m := parseProviderModel(tt.input)
			if p != tt.provider {
				t.Errorf("parseProviderModel(%q) provider = %q, want %q", tt.input, p, tt.provider)
			}
			if m != tt.model {
				t.Errorf("parseProviderModel(%q) model = %q, want %q", tt.input, m, tt.model)
			}
		})
	}
}

func TestInferEmbeddingProvider(t *testing.T) {
	tests := []struct {
		name      string
		geminiKey string
		openaiKey string
		expected  string
	}{
		{name: "gemini preferred when available", geminiKey: "g-key", openaiKey: "o-key", expected: "gemini"},
		{name: "openai fallback", openaiKey: "o-key", expected: "openai"},
		{name: "none available", expected: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Embed.Gemini.APIKey = tt.geminiKey
			cfg.Embed.OpenAI.APIKey = tt.openaiKey
			result := inferEmbeddingProvider(cfg)
			if result != tt.expected {
				t.Errorf("inferEmbeddingProvider() = %q, want %q", result, tt.expected)
			}
		})
	}
}
