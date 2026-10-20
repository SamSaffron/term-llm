package ask

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// NewProvider constructs the selected ask provider after model selection.
func NewProvider(cfg *config.Config, fast bool) (core.Provider, error) {
	if fast {
		key, model, ok := llm.ResolveFastTarget(cfg, cfg.DefaultProvider)
		if !ok {
			return nil, fmt.Errorf("no fast provider configured for %q", cfg.DefaultProvider)
		}
		return llm.NewProviderByName(cfg, key, model)
	}
	return llm.NewProvider(cfg)
}
