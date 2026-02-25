package llm

import (
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// ProviderModels contains the curated list of common models per LLM provider type
var ProviderModels = map[string][]string{
	"anthropic": {
		// Claude 4.6 (current)
		"claude-sonnet-4-6",
		"claude-sonnet-4-6-thinking",
		"claude-opus-4-6",
		"claude-opus-4-6-thinking",
		"claude-haiku-4-5",
		"claude-haiku-4-5-thinking",
	},
	"openai": {
		"gpt-5.3-codex",
		"gpt-5.2-codex",
		"gpt-5.2",
		"gpt-5.1",
		"gpt-5",
		"gpt-5-mini",
		"gpt-5-nano",
		"o3-mini",
	},
	"chatgpt": {
		// Uses ChatGPT backend API with native OAuth
		"gpt-5.3-codex",
		"gpt-5.3-codex-spark",
		"gpt-5.2-codex",
		"gpt-5.2",
		"gpt-5.1-codex-max",
		"gpt-5.1-codex",
		"gpt-5.1-codex-mini",
		"gpt-5.1",
		"gpt-5-codex",
		"gpt-5-codex-mini",
		"gpt-5",
	},
	"copilot": {
		// Uses GitHub Copilot API with device code OAuth
		// Run 'term-llm models --provider copilot' for live list
		// gpt-4.1 is free (no premium requests)
		"gpt-4.1",
		// OpenAI Codex models
		"gpt-5.2-codex",
		"gpt-5.1-codex",
		"gpt-5.1-codex-max",
		"gpt-5.1-codex-mini",
		// OpenAI standard
		"gpt-5.2",
		"gpt-5.1",
		"gpt-5-mini",
		// Anthropic Claude
		"claude-opus-4.5",
		"claude-sonnet-4.5",
		"claude-sonnet-4",
		"claude-haiku-4.5",
		// Google Gemini
		"gemini-3-pro",
		"gemini-3-flash",
		// Other
		"grok-code-fast-1",
		"raptor-mini",
	},
	"openrouter": {
		"x-ai/grok-code-fast-1",
	},
	"gemini": {
		"gemini-3-pro-preview",
		"gemini-3-pro-preview-thinking",
		"gemini-3-flash-preview",
		"gemini-3-flash-preview-thinking",
		"gemini-2.5-flash",
		"gemini-2.5-flash-lite",
	},
	"gemini-cli": {
		"gemini-3-pro-preview",
		"gemini-3-pro-preview-thinking",
		"gemini-3-flash-preview",
		"gemini-3-flash-preview-thinking",
		"gemini-2.5-flash",
		"gemini-2.5-flash-lite",
	},
	"zen": {
		"big-pickle",
		"glm-4.7-free",
		"trinity-large-preview-free",
		"kimi-k2.5-free",
		"minimax-m2.1-free",
		"gpt-5-nano",
	},
	"claude-bin": {
		"opus",
		"opus-low",
		"opus-medium",
		"opus-high",
		"opus-max",
		"sonnet",
		"haiku",
	},
	"xai": {
		// Grok 4.1 (latest, 2M context)
		"grok-4-1-fast",
		"grok-4-1-fast-reasoning",
		"grok-4-1-fast-non-reasoning",
		// Grok 4 (256K-2M context)
		"grok-4",
		"grok-4-fast-reasoning",
		"grok-4-fast-non-reasoning",
		// Grok 3 (131K context)
		"grok-3",
		"grok-3-fast",
		"grok-3-mini",
		"grok-3-mini-fast",
		// Specialized
		"grok-code-fast-1",
		// Grok 2
		"grok-2",
	},
	"venice": {
		"venice-uncensored",
		"olafangensan-glm-4.7-flash-heretic",
		"zai-org-glm-4.7-flash",
		"zai-org-glm-5",
		"zai-org-glm-4.7",
		"qwen3-4b",
		"mistral-31-24b",
		"qwen3-235b-a22b-thinking-2507",
		"qwen3-235b-a22b-instruct-2507",
		"qwen3-next-80b",
		"qwen3-coder-480b-a35b-instruct",
		"hermes-3-llama-3.1-405b",
		"google-gemma-3-27b-it",
		"grok-41-fast",
		"gemini-3-pro-preview",
		"gemini-3-1-pro-preview",
		"gemini-3-flash-preview",
		"claude-opus-4-6",
		"claude-opus-45",
		"claude-sonnet-4-6",
		"claude-sonnet-45",
		"openai-gpt-oss-120b",
		"kimi-k2-thinking",
		"kimi-k2-5",
		"deepseek-v3.2",
		"llama-3.2-3b",
		"llama-3.3-70b",
		"openai-gpt-52",
		"openai-gpt-52-codex",
		"openai-gpt-53-codex",
		"minimax-m21",
		"minimax-m25",
		"grok-code-fast-1",
		"qwen3-vl-235b-a22b",
	},
}

var ImageProviderModels = map[string][]string{
	"debug":      {"random"},
	"gemini":     {"gemini-2.5-flash-image", "gemini-3-pro-image-preview"},
	"openai":     {"gpt-image-1.5", "gpt-image-1-mini"},
	"flux":       {"flux-2-pro", "flux-kontext-pro", "flux-2-max"},
	"openrouter": {"google/gemini-2.5-flash-image", "google/gemini-3-pro-image-preview", "openai/gpt-5-image", "openai/gpt-5-image-mini", "bytedance-seed/seedream-4.5", "black-forest-labs/flux.2-pro"},
}

// defaultEffortVariants are the standard effort levels for reasoning-capable models.
var defaultEffortVariants = []string{"low", "medium", "high", "xhigh"}

// EffortVariantsFor returns the effort suffixes for a model, or nil if none.
// All GPT-5 family models are reasoning-capable and support effort levels.
func EffortVariantsFor(model string) []string {
	if strings.HasPrefix(model, "gpt-5") {
		return defaultEffortVariants
	}
	return nil
}

// ExpandWithEffortVariants expands a model list by appending effort variants
// after each base model. Used for tab-completion where all variants are needed.
func ExpandWithEffortVariants(models []string) []string {
	var expanded []string
	for _, m := range models {
		expanded = append(expanded, m)
		if variants := EffortVariantsFor(m); len(variants) > 0 {
			for _, v := range variants {
				expanded = append(expanded, m+"-"+v)
			}
		}
	}
	return expanded
}

// GetBuiltInProviderNames returns the built-in provider type names
func GetBuiltInProviderNames() []string {
	return []string{"anthropic", "openai", "chatgpt", "copilot", "openrouter", "gemini", "gemini-cli", "zen", "claude-bin", "xai", "venice"}
}

// GetProviderNames returns valid provider names from config plus built-in types.
// If cfg is nil, returns only built-in provider names.
func GetProviderNames(cfg *config.Config) []string {
	names := make(map[string]bool)

	// Add built-in provider names
	for _, name := range GetBuiltInProviderNames() {
		names[name] = true
	}

	// Add configured provider names
	if cfg != nil {
		for name := range cfg.Providers {
			names[name] = true
		}
	}

	// Convert to sorted slice
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// GetImageProviderNames returns valid provider names for image generation
func GetImageProviderNames() []string {
	return []string{"debug", "gemini", "openai", "flux", "openrouter"}
}

// GetProviderCompletions returns completions for the --provider flag
// It handles both provider-only and provider:model completion scenarios.
// For LLM providers, pass a config to include custom provider names.
func GetProviderCompletions(toComplete string, isImage bool, cfg *config.Config) []string {
	var providerNames []string
	var modelMap map[string][]string

	if isImage {
		providerNames = GetImageProviderNames()
		modelMap = ImageProviderModels
	} else {
		providerNames = GetProviderNames(cfg)
		modelMap = ProviderModels
	}

	// Check if user has typed a colon (wants model completion)
	if strings.Contains(toComplete, ":") {
		parts := strings.SplitN(toComplete, ":", 2)
		provider := parts[0]
		modelPrefix := parts[1]

		// Get models for completion
		var models []string

		// Check if config has a models list for this provider
		var configModels []string
		var configModel string
		if cfg != nil {
			if providerCfg, ok := cfg.Providers[provider]; ok {
				configModels = providerCfg.Models
				configModel = providerCfg.Model
			}
		}

		if len(configModels) > 0 {
			// Use config-defined models list, plus configured model (deduped)
			seen := make(map[string]bool)
			if configModel != "" {
				models = append(models, configModel)
				seen[configModel] = true
			}
			for _, m := range configModels {
				if !seen[m] {
					models = append(models, m)
					seen[m] = true
				}
			}
		} else {
			providerType := string(config.InferProviderType(provider, ""))

			// For LLM (non-image) openrouter, fetch models from API cache
			if !isImage && (providerType == "openrouter" || provider == "openrouter") {
				var apiKey string
				if cfg != nil {
					if providerCfg, ok := cfg.Providers[provider]; ok {
						apiKey = providerCfg.ResolvedAPIKey
					}
				}
				if cachedModels := GetCachedOpenRouterModels(apiKey); len(cachedModels) > 0 {
					models = cachedModels
				} else {
					models = modelMap["openrouter"]
				}
			} else {
				var ok bool
				models, ok = modelMap[providerType]
				if !ok {
					models, ok = modelMap[provider]
				}
				if !ok && configModel != "" {
					models = []string{configModel}
				} else if !ok {
					return nil
				}
			}
		}

		// Expand effort variants for tab-completion
		if !isImage {
			models = ExpandWithEffortVariants(models)
		}

		// Filter by prefix and return as provider:model
		var completions []string
		for _, model := range models {
			if strings.HasPrefix(model, modelPrefix) {
				completions = append(completions, provider+":"+model)
			}
		}
		return completions
	}

	// No colon - offer provider names (filtered by what user typed)
	var completions []string
	for _, name := range providerNames {
		if strings.HasPrefix(name, toComplete) {
			completions = append(completions, name)
		}
	}
	return completions
}
