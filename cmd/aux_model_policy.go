package cmd

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
)

// auxProviderForPolicy checks every candidate, including the default-provider
// fallback. Automatic work can use the already-admitted main model instead.
func auxProviderForPolicy(cfg *config.Config, policy modelpolicy.Policy, main modelSelection, keys ...string) (llm.Provider, error) {
	if cfg == nil {
		return nil, nil
	}
	var lastErr error
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		provider, model, ok := llm.ResolveFastTarget(cfg, key)
		if !ok || policy.CheckWithConfig(cfg, "auxiliary", provider, model) != nil {
			continue
		}
		chosen, err := llm.NewProviderByName(cfg, provider, model)
		if err == nil && chosen != nil {
			return chosen, nil
		}
		if err != nil {
			lastErr = fmt.Errorf("auxiliary provider %s:%s: %w", provider, model, err)
		}
		if policy.Restricted() {
			break
		}
	}
	if policy.Restricted() && main.Provider != "" && main.Model != "" {
		return mainProviderForPolicy(cfg, policy, main)
	}
	return nil, lastErr
}

func mainProviderForPolicy(cfg *config.Config, policy modelpolicy.Policy, main modelSelection) (llm.Provider, error) {
	if main.Provider == "" || main.Model == "" {
		return nil, fmt.Errorf("auxiliary fallback has no main model")
	}
	if err := policy.CheckWithConfig(cfg, "auxiliary", main.Provider, main.Model); err != nil {
		return nil, err
	}
	provider, err := llm.NewProviderByName(cfg, main.Provider, main.Model)
	if err != nil {
		return nil, fmt.Errorf("auxiliary fallback: %w", err)
	}
	return provider, nil
}
