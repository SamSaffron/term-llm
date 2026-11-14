package cmd

import (
	"github.com/term-llm/term-llm/internal/app/runtime"
	"github.com/term-llm/term-llm/internal/config"
)

func resolveAgentModelOverride(cfg *config.Config, model string) (provider string, resolvedModel string, resolved bool, err error) {
	return runtime.ResolveAgentModelOverride(cfg, model)
}

func applyAgentModelOverride(cfg *config.Config, model string) error {
	return runtime.ApplyAgentModelOverride(cfg, model)
}
