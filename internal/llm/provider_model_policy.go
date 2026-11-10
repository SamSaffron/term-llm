package llm

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// CheckProviderModelAllowed enforces the operator's provider-wide model ceiling.
// Configured aliases are matched by upstream identity; moving backend aliases
// such as Claude Code's "opus" are not resolved to a pinned generation.
func CheckProviderModelAllowed(cfg *config.Config, provider, model string) error {
	if cfg == nil {
		return nil
	}
	pc := cfg.Providers[provider]
	if len(pc.AllowedModels) == 0 {
		return nil
	}
	if err := config.ValidateProviderAllowedModels(provider, pc.AllowedModels); err != nil {
		return err
	}
	selected := providerPolicyModel(cfg, provider, model)
	for _, allowed := range pc.AllowedModels {
		allowed = config.UpstreamModelForProviderModel(cfg, provider, allowed)
		if IsModelOrEffortVariantForConfig(cfg, provider, selected, allowed) {
			return nil
		}
	}
	return fmt.Errorf("model %q is not allowed by providers.%s.allowed_models (allowed: %s)", provider+":"+selected, provider, strings.Join(pc.AllowedModels, ", "))
}

func providerPolicyModel(cfg *config.Config, provider, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		pc := cfg.Providers[provider]
		model = strings.TrimSpace(pc.Model)
		if model == "" {
			model = config.DefaultProviderModel(string(config.InferProviderType(provider, pc.Type)))
		}
	}
	return config.UpstreamModelForProviderModel(cfg, provider, model)
}

// The existing request wrapper owns dispatch and forwards optional capabilities,
// so adding admission here preserves CLI tool loops, state and helper forks.
func wrapWithProviderModelPolicy(cfg *config.Config, name, model string, provider core.Provider, retry RetryConfig) core.Provider {
	wrapped := &RetryProvider{inner: provider, config: normalizeRetryConfig(retry)}
	pc := cfg.Providers[name]
	if len(pc.AllowedModels) == 0 {
		return wrapped
	}
	pc.AllowedModels = append([]string(nil), pc.AllowedModels...)
	pc.Models = append([]string(nil), pc.Models...)
	pc.ModelConfigs = append([]config.ProviderModelConfig(nil), pc.ModelConfigs...)
	for i := range pc.ModelConfigs {
		pc.ModelConfigs[i].ReasoningEfforts = append([]string(nil), pc.ModelConfigs[i].ReasoningEfforts...)
	}
	// Freeze the selected default: later ApplyOverrides calls must not change the
	// fallback identity of a provider already constructed for another session.
	pc.Model = providerPolicyModel(cfg, name, model)
	snapshot := &config.Config{Providers: map[string]config.ProviderConfig{name: pc}}
	wrapped.checkModel = func(model, effort string) error {
		model = providerPolicyModel(snapshot, name, model)
		if effort = strings.TrimSpace(effort); effort != "" {
			base, _ := BaseModelAndEffortForProvider(name, model)
			if entry, ok := config.ModelConfigForProviderModel(snapshot, name, model); ok {
				base = trimDeclaredModelEffort(model, entry)
			}
			model = base + "-" + effort
		}
		return CheckProviderModelAllowed(snapshot, name, model)
	}
	return wrapped
}
