package llm

import (
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// ProviderModels contains the curated list of common models per LLM provider type
var ProviderModels = map[string][]string{
	"anthropic": {
		// Claude 4.5 (current)
		"claude-sonnet-4-5",
		"claude-sonnet-4-5-thinking",
		"claude-opus-4-5",
		"claude-opus-4-5-thinking",
		"claude-haiku-4-5",
		"claude-haiku-4-5-thinking",
	},
	"openai": {
		"gpt-5.2",
		"gpt-5.2-high",
		"gpt-5.2-codex",
		"gpt-5.2-codex-medium",
		"gpt-5.2-codex-high",
		"gpt-5.2-codex-xhigh",
		"gpt-4.1",
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
	"zen": {
		"glm-4.7-free",
		"grok-code",
		"minimax-m2.1-free",
		"big-pickle",
		"gpt-5-nano",
	},
	"claude-bin": {
		"opus",
		"sonnet",
		"haiku",
	},
}

var ImageProviderModels = map[string][]string{
	"gemini": {"gemini-2.5-flash-image", "gemini-3-pro-image-preview"},
	"openai": {"gpt-image-1.5", "gpt-image-1-mini"},
	"flux":   {"flux-2-pro", "flux-kontext-pro", "flux-2-max"},
}

// GetBuiltInProviderNames returns the built-in provider type names
func GetBuiltInProviderNames() []string {
	return []string{"anthropic", "openai", "openrouter", "gemini", "zen", "claude-bin"}
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
	return []string{"gemini", "openai", "flux"}
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

			if providerType == "openrouter" || provider == "openrouter" {
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
