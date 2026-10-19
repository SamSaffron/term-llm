package runtime

import (
	appagent "github.com/samsaffron/term-llm/internal/app/agent"
	"github.com/samsaffron/term-llm/internal/config"
)

// ResolveAgentModelOverride resolves an agent model override without mutating cfg.
func ResolveAgentModelOverride(cfg *config.Config, model string) (provider, resolvedModel string, resolved bool, err error) {
	return appagent.ResolveModelOverride(cfg, model)
}

// ApplyAgentModelOverride applies an agent model override to cfg.
func ApplyAgentModelOverride(cfg *config.Config, model string) error {
	return appagent.ApplyModelOverride(cfg, model)
}
