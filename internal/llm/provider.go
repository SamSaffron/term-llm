package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/input"
)

// CommandSuggestion represents a single command suggestion from the LLM
type CommandSuggestion struct {
	Command     string `json:"command"`
	Explanation string `json:"explanation"`
	Likelihood  int    `json:"likelihood"` // 1-10, how likely this matches user intent
}

// suggestionsResponse is the common response format for all providers
type suggestionsResponse struct {
	Suggestions []CommandSuggestion `json:"suggestions"`
}

// Provider is the interface for LLM providers
type Provider interface {
	// Name returns the provider name for logging/debugging
	Name() string

	// SuggestCommands generates command suggestions based on user input
	SuggestCommands(ctx context.Context, req SuggestRequest) ([]CommandSuggestion, error)

	// StreamResponse streams a text response for the ask command
	StreamResponse(ctx context.Context, req AskRequest, output chan<- string) error
}

// AskRequest contains parameters for asking a question
type AskRequest struct {
	Question     string
	Instructions string // Custom system prompt
	EnableSearch bool
	Debug        bool
	Files        []input.FileContent // Files to include as context
	Stdin        string              // Content piped via stdin
}

// SuggestRequest contains all parameters for a suggestion request
type SuggestRequest struct {
	UserInput      string
	Shell          string
	Instructions   string              // Custom user instructions/context
	NumSuggestions int                 // Number of suggestions to request (default 3)
	EnableSearch   bool
	Debug          bool
	Files          []input.FileContent // Files to include as context
	Stdin          string              // Content piped via stdin
}

// EditToolCall represents a single edit tool call (find/replace)
type EditToolCall struct {
	FilePath  string `json:"file_path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// EditToolProvider is an optional interface for providers that support the edit tool
type EditToolProvider interface {
	GetEdits(ctx context.Context, systemPrompt, userPrompt string, debug bool) ([]EditToolCall, error)
}

// UnifiedDiffProvider is an optional interface for providers that support unified diff format.
// This is more efficient for models fine-tuned on single tool calls (e.g., Codex models).
type UnifiedDiffProvider interface {
	GetUnifiedDiff(ctx context.Context, systemPrompt, userPrompt string, debug bool) (string, error)
}

// IsCodexModel returns true if the model name indicates a Codex model
// which works better with unified diff format (single tool call).
func IsCodexModel(model string) bool {
	model = strings.ToLower(model)
	return strings.Contains(model, "codex")
}

// ParseProviderModel parses "provider:model" or just "provider" from a flag value.
// Returns (provider, model, error). Model will be empty if not specified.
func ParseProviderModel(s string) (string, string, error) {
	parts := strings.SplitN(s, ":", 2)
	provider := parts[0]
	model := ""
	if len(parts) == 2 {
		model = parts[1]
	}
	// Validate provider name
	switch provider {
	case "anthropic", "openai", "gemini", "zen":
		return provider, model, nil
	default:
		return "", "", fmt.Errorf("unknown provider: %s (valid: anthropic, openai, gemini, zen)", provider)
	}
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
		// Use CodexProvider when using Codex OAuth credentials (has account ID)
		if cfg.OpenAI.AccountID != "" {
			return NewCodexProvider(cfg.OpenAI.APIKey, cfg.OpenAI.Model, cfg.OpenAI.AccountID), nil
		}
		return NewOpenAIProvider(cfg.OpenAI.APIKey, cfg.OpenAI.Model), nil

	case "gemini":
		// Use CodeAssistProvider when using gemini-cli OAuth credentials
		if cfg.Gemini.OAuthCreds != nil {
			creds := &GeminiOAuthCredentials{
				AccessToken:  cfg.Gemini.OAuthCreds.AccessToken,
				RefreshToken: cfg.Gemini.OAuthCreds.RefreshToken,
				ExpiryDate:   cfg.Gemini.OAuthCreds.ExpiryDate,
			}
			return NewCodeAssistProvider(creds, cfg.Gemini.Model), nil
		}
		if cfg.Gemini.APIKey == "" {
			return nil, fmt.Errorf("gemini API key not configured. Set GEMINI_API_KEY or add to config")
		}
		return NewGeminiProvider(cfg.Gemini.APIKey, cfg.Gemini.Model, false), nil

	case "zen":
		// OpenCode Zen - free tier works without API key
		return NewZenProvider(cfg.Zen.APIKey, cfg.Zen.Model), nil

	default:
		return nil, fmt.Errorf("unknown provider: %s", cfg.Provider)
	}
}
