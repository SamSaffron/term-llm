package llm

import (
	"context"
	"fmt"

	"github.com/samsaffron/term-llm/internal/config"
)

// CommandSuggestion represents a single command suggestion from the LLM
type CommandSuggestion struct {
	Command     string `json:"command"`
	Explanation string `json:"explanation"`
	Likelihood  int    `json:"likelihood"` // 1-10, how likely this matches user intent
}

// Provider is the interface for LLM providers
type Provider interface {
	SuggestCommands(ctx context.Context, userInput string, shell string, systemContext string) ([]CommandSuggestion, error)
}

// NewProvider creates a new LLM provider based on the config
func NewProvider(cfg *config.Config) (Provider, error) {
	switch cfg.Provider {
	case "anthropic":
		if cfg.Anthropic.APIKey == "" {
			return nil, fmt.Errorf("anthropic API key not configured. Set ANTHROPIC_API_KEY or add to config")
		}
		return NewAnthropicProvider(cfg.Anthropic.APIKey, cfg.Anthropic.Model), nil
	case "openai":
		if cfg.OpenAI.APIKey == "" {
			return nil, fmt.Errorf("openai API key not configured. Set OPENAI_API_KEY or add to config")
		}
		return NewOpenAIProvider(cfg.OpenAI.APIKey, cfg.OpenAI.Model), nil
	default:
		return nil, fmt.Errorf("unknown provider: %s", cfg.Provider)
	}
}
