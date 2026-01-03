package llm

import "strings"

// ProviderModels contains the curated list of common models per LLM provider
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
	"gemini": {
		"gemini-3-pro-preview",
		"gemini-3-flash-preview",
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
}

var ImageProviderModels = map[string][]string{
	"gemini": {"gemini-2.5-flash-image", "gemini-3-pro-image-preview"},
	"openai": {"gpt-image-1.5", "gpt-image-1-mini"},
	"flux":   {"flux-2-pro", "flux-kontext-pro", "flux-2-max"},
}

// GetProviderNames returns valid provider names for LLM
func GetProviderNames() []string {
	return []string{"anthropic", "openai", "gemini", "zen"}
}

// GetImageProviderNames returns valid provider names for image generation
func GetImageProviderNames() []string {
	return []string{"gemini", "openai", "flux"}
}

// GetProviderCompletions returns completions for the --provider flag
// It handles both provider-only and provider:model completion scenarios
func GetProviderCompletions(toComplete string, isImage bool) []string {
	var providerNames []string
	var modelMap map[string][]string

	if isImage {
		providerNames = GetImageProviderNames()
		modelMap = ImageProviderModels
	} else {
		providerNames = GetProviderNames()
		modelMap = ProviderModels
	}

	// Check if user has typed a colon (wants model completion)
	if strings.Contains(toComplete, ":") {
		parts := strings.SplitN(toComplete, ":", 2)
		provider := parts[0]
		modelPrefix := parts[1]

		// Get models for this provider
		models, ok := modelMap[provider]
		if !ok {
			return nil
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
