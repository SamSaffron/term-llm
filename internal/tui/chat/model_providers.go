package chat

import (
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm"
)

// ProviderInfo holds provider and model information
type ProviderInfo struct {
	Name   string
	Models []string
}

// providerHasLocalCredentials detects local login state for built-ins; tests
// replace it to avoid depending on the developer's machine.
var providerHasLocalCredentials = llm.DefaultProviderCredentials.Has

// GetAvailableProviders returns configured providers with their models in
// consistent order: the same set the web UI picker and `term-llm providers`
// treat as configured under provider_discovery (see
// config.ProviderConfiguredVia).
func GetAvailableProviders(cfg *config.Config) []ProviderInfo {
	var providers []ProviderInfo
	seen := make(map[string]bool)

	// Add built-in providers first, merging any configured model
	for _, name := range config.GetBuiltInProviderNames() {
		seen[name] = true
		if cfg.ProviderConfiguredVia(name, providerHasLocalCredentials) == "" {
			continue
		}
		curated := llm.ProviderModelIDs(name)
		curated = llm.ProviderModelIDsForDiscovery(cfg, name, curated)
		if len(curated) == 0 {
			continue
		}
		var defaultModel string
		if cfg != nil {
			if providerCfg, ok := cfg.Providers[name]; ok {
				defaultModel = providerCfg.Model
			}
		}
		ordered := llm.SortModelIDsByPopularity(name, defaultModel, curated)
		providers = append(providers, ProviderInfo{
			Name:   name,
			Models: llm.ExpandProviderModelIDsForDiscovery(cfg, name, ordered),
		})
	}

	// Add custom configured providers from config
	if cfg != nil {
		for name, providerCfg := range cfg.Providers {
			if seen[name] || cfg.ProviderConfiguredVia(name, nil) == "" {
				continue
			}
			source := providerCfg.Models
			if len(source) == 0 {
				source = llm.ResolveProviderModelIDs(name)
			}
			source = llm.ProviderModelIDsForDiscovery(cfg, name, source)
			ordered := llm.SortModelIDsByPopularity(name, providerCfg.Model, source)
			if len(ordered) == 0 {
				continue
			}
			providers = append(providers, ProviderInfo{
				Name:   name,
				Models: llm.ExpandProviderModelIDsForDiscovery(cfg, name, ordered),
			})
		}
	}

	return providers
}
