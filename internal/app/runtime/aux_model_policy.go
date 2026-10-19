package runtime

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
)

// AuxProviderForPolicy resolves the first allowed auxiliary model, falling back
// to the already-admitted main model for restricted policies.
func AuxProviderForPolicy(cfg *config.Config, policy modelpolicy.Policy, main ModelSelection, keys ...string) (core.Provider, error) {
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
		return MainProviderForPolicy(cfg, policy, main)
	}
	return nil, lastErr
}

// MainProviderForPolicy constructs the admitted main provider for auxiliary use.
func MainProviderForPolicy(cfg *config.Config, policy modelpolicy.Policy, main ModelSelection) (core.Provider, error) {
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
