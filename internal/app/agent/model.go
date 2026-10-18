// Package agent holds the agent model-selection logic extracted from cmd.
package agent

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

// FastModelAlias selects the configured fast model of the active provider.
const FastModelAlias = "fast"

func IsFastModelAlias(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), FastModelAlias)
}

// ResolveModelOverride resolves special agent model aliases into concrete
// provider/model overrides. Today the only alias is "fast", which selects the
// configured fast provider/model for the currently active provider.
func ResolveModelOverride(cfg *config.Config, model string) (provider string, resolvedModel string, resolved bool, err error) {
	model = strings.TrimSpace(model)
	if !IsFastModelAlias(model) {
		return "", model, false, nil
	}
	if cfg == nil {
		return "", "", true, fmt.Errorf("cannot resolve agent model %q without config", FastModelAlias)
	}

	providerKey := strings.TrimSpace(cfg.DefaultProvider)
	if providerKey == "" {
		return "", "", true, fmt.Errorf("cannot resolve agent model %q without an active provider", FastModelAlias)
	}

	targetKey, targetModel, ok := llm.ResolveFastTarget(cfg, providerKey)
	if !ok {
		return "", "", true, fmt.Errorf("no fast model configured for provider %q", providerKey)
	}
	return targetKey, targetModel, true, nil
}

func ApplyModelOverride(cfg *config.Config, model string) error {
	provider, resolvedModel, resolved, err := ResolveModelOverride(cfg, model)
	if err != nil {
		return err
	}
	if resolved {
		cfg.ApplyOverrides(provider, resolvedModel)
		return nil
	}
	if strings.Contains(resolvedModel, ":") {
		if provider, parsedModel, err := llm.ParseProviderModel(resolvedModel, cfg); err == nil {
			cfg.ApplyOverrides(provider, parsedModel)
			return nil
		}
		// Some providers use colon-tagged model names (for example, local model
		// tags). If the prefix is not a known provider, treat the whole value as
		// a plain model name and let provider creation validate it later.
	}
	if resolvedModel != "" {
		cfg.ApplyOverrides("", resolvedModel)
	}
	return nil
}
