package cmd

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

func checkAgentModel(agent *agents.Agent, cfg *config.Config, fast bool) error {
	if agent == nil || len(agent.AllowedModels) == 0 {
		return nil
	}
	provider, model := strings.TrimSpace(cfg.DefaultProvider), strings.TrimSpace(activeModel(cfg))
	if fast {
		var err error
		provider, model, _, err = resolveAgentModelOverride(cfg, agentFastModelAlias)
		if err != nil {
			return err
		}
	}
	return agent.CheckModel(provider, model)
}

func applyAgentProviderModelPolicy(cfg *config.Config, cmdProvider, cmdModel, providerFlag string, agent *agents.Agent, fast bool) error {
	agentProvider, agentModel := "", ""
	if agent != nil {
		agentProvider, agentModel = agent.Provider, agent.Model
	}
	if err := applyProviderOverridesWithAgent(cfg, cmdProvider, cmdModel, providerFlag, agentProvider, agentModel); err != nil {
		return err
	}
	return checkAgentModel(agent, cfg, fast)
}

const agentFastModelAlias = "fast"

func isAgentFastModelAlias(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), agentFastModelAlias)
}

// resolveAgentModelOverride resolves special agent model aliases into concrete
// provider/model overrides. Today the only alias is "fast", which selects the
// configured fast provider/model for the currently active provider.
func resolveAgentModelOverride(cfg *config.Config, model string) (provider string, resolvedModel string, resolved bool, err error) {
	model = strings.TrimSpace(model)
	if !isAgentFastModelAlias(model) {
		return "", model, false, nil
	}
	if cfg == nil {
		return "", "", true, fmt.Errorf("cannot resolve agent model %q without config", agentFastModelAlias)
	}

	providerKey := strings.TrimSpace(cfg.DefaultProvider)
	if providerKey == "" {
		return "", "", true, fmt.Errorf("cannot resolve agent model %q without an active provider", agentFastModelAlias)
	}

	targetKey := providerKey
	targetModel := ""
	if pc, ok := cfg.Providers[providerKey]; ok {
		if strings.TrimSpace(pc.FastProvider) != "" {
			targetKey = strings.TrimSpace(pc.FastProvider)
		}
		targetModel = strings.TrimSpace(pc.FastModel)
	}

	if targetModel == "" {
		var explicitType config.ProviderType
		if targetCfg, ok := cfg.Providers[targetKey]; ok {
			explicitType = targetCfg.Type
		}
		providerType := string(config.InferProviderType(targetKey, explicitType))
		targetModel = llm.ProviderFastModels[providerType]
	}

	if targetModel == "" {
		return "", "", true, fmt.Errorf("no fast model configured for provider %q", providerKey)
	}
	return targetKey, targetModel, true, nil
}

func applyAgentModelOverride(cfg *config.Config, model string) error {
	provider, resolvedModel, resolved, err := resolveAgentModelOverride(cfg, model)
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
