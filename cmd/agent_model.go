package cmd

import (
	"github.com/samsaffron/term-llm/internal/app/agent"
	"github.com/samsaffron/term-llm/internal/config"
)

func resolveAgentModelOverride(cfg *config.Config, model string) (provider string, resolvedModel string, resolved bool, err error) {
	return agent.ResolveModelOverride(cfg, model)
}

func applyAgentModelOverride(cfg *config.Config, model string) error {
	return agent.ApplyModelOverride(cfg, model)
}
