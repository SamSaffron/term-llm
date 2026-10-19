package llm

import (
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/providers/anthropic"
	"github.com/samsaffron/term-llm/internal/llm/providers/clibin"
	rp "github.com/samsaffron/term-llm/internal/llm/providers/responsesproviders"
)

// cachedProviderModels contains provider-specific offline/live catalog probes.
// Curated fallbacks remain in ProviderModels.
var cachedProviderModels = map[string]func(string) []string{
	"chatgpt": func(provider string) []string {
		if ids := rp.GetCachedChatGPTModels(); len(ids) > 0 {
			return ids
		}
		return nil
	},
	"copilot": func(provider string) []string {
		return rp.GetCachedCopilotModels()
	},
	"opencode-go": func(provider string) []string {
		return rp.GetCachedOpenCodeGoModels()
	},
	"zen": func(provider string) []string {
		if ids := rp.GetCachedZenModels(); len(ids) > 0 {
			return ids
		}
		return nil
	},
	"anthropic": func(provider string) []string {
		// The cached catalog is the public API's; custom-named Anthropic
		// gateways keep the curated list (or their configured models).
		if provider == "anthropic" && strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")) == "" {
			if ids := anthropic.GetCachedAnthropicModels(); len(ids) > 0 {
				return ids
			}
		}
		return nil
	},
	"agy-bin": func(provider string) []string {
		if ids := clibin.GetCachedAgyBinModels(); len(ids) > 0 {
			return ids
		}
		return nil
	},
	"cursor-bin": func(provider string) []string {
		if ids := clibin.GetCachedCursorBinModels(); len(ids) > 0 {
			return ids
		}
		return nil
	},
	"grok": func(provider string) []string {
		if models, _, err := rp.CachedGrokModels(); err == nil {
			ids := make([]string, 0, len(models))
			for _, model := range models {
				ids = append(ids, model.ID)
			}
			return ids
		}
		return nil
	},
	"grok-bin": func(provider string) []string {
		if ids := clibin.GetCachedGrokBinModels(); len(ids) > 0 {
			return ids
		}
		return nil
	},
}

// ProviderModelIDs returns model IDs for a built-in provider. Dynamic providers
// prefer cached catalogs and otherwise use their curated starter lists.
// For callers that might receive a custom alias name, use ResolveProviderModelIDs.
func ProviderModelIDs(provider string) []string {
	if cached := cachedProviderModels[resolveProviderType(provider)]; cached != nil {
		ids := cached(provider)
		if spec, ok := config.BuiltinProviderOfType(config.ProviderType(resolveProviderType(provider))); ok && spec.CacheOnly {
			return ids
		}
		if ids != nil {
			return ids
		}
	}
	entries := ProviderModels[provider]
	if entries == nil {
		return nil
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

// ResolveProviderModelIDs returns curated model IDs for a provider, resolving
// custom aliases (e.g., "acme" → "venice") via registered provider aliases
// and built-in type inference.
func ResolveProviderModelIDs(name string) []string {
	if ids := ProviderModelIDs(name); len(ids) > 0 {
		return ids
	}
	// Resolve via alias or type inference
	resolved := resolveProviderType(name)
	if resolved != name {
		return ProviderModelIDs(resolved)
	}
	return nil
}
