package config

import (
	"fmt"
	"strings"
	"unicode"
)

// ValidateProviderAllowedModels validates literal model names, not provider:model
// entries or glob patterns. Colons remain valid for model IDs such as qwen:8b.
func ValidateProviderAllowedModels(name string, allowed []string) error {
	for _, model := range allowed {
		if model == "" || strings.IndexFunc(model, unicode.IsSpace) >= 0 || strings.ContainsAny(model, "*?") {
			return fmt.Errorf("providers.%s.allowed_models: invalid model %q; use literal model names without whitespace or wildcards", name, model)
		}
	}
	return nil
}

func validateProviderModels(cfg *Config) error {
	for name, pc := range cfg.Providers {
		if err := ValidateProviderAllowedModels(name, pc.AllowedModels); err != nil {
			return err
		}
	}
	return validateProviderModelReasoning(cfg)
}

func validateProviderModelReasoning(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	for provider, pc := range cfg.Providers {
		for _, entry := range pc.ModelConfigs {
			defaultEffort := strings.ToLower(strings.TrimSpace(entry.DefaultReasoningEffort))
			if defaultEffort == "" {
				continue
			}
			supported := false
			for _, effort := range entry.ReasoningEfforts {
				if strings.EqualFold(strings.TrimSpace(effort), defaultEffort) {
					supported = true
					break
				}
			}
			if !supported {
				return fmt.Errorf("providers.%s model %q default_reasoning_effort %q is not listed in reasoning_efforts", provider, entry.DisplayName(), entry.DefaultReasoningEffort)
			}
		}
	}
	return nil
}

// providerAllowedModelsForSave preserves existing operator policy when setup
// saves a fresh Config for a retained provider. Removed providers stay removed.
// An explicit non-nil empty slice deliberately clears the restriction.
func providerAllowedModelsForSave(cfg *Config) (map[string][]string, error) {
	path, err := FindConfigFile()
	if err != nil {
		return nil, err
	}
	file, err := readFileSettings(path)
	if err != nil {
		return nil, err
	}
	var existing Config
	if err := decodeSettings(file.mergedSettings(), &existing, path); err != nil {
		return nil, err
	}
	allowed := make(map[string][]string)
	for name, pc := range existing.Providers {
		if _, retained := cfg.Providers[name]; retained && len(pc.AllowedModels) > 0 {
			allowed[name] = pc.AllowedModels
		}
	}
	for name, pc := range cfg.Providers {
		if pc.AllowedModels != nil {
			allowed[name] = pc.AllowedModels
		}
	}
	for name, models := range allowed {
		if err := ValidateProviderAllowedModels(name, models); err != nil {
			return nil, err
		}
	}
	return allowed, nil
}
