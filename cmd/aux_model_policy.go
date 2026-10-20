package cmd

import (
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
)

func auxProviderForPolicy(cfg *config.Config, policy modelpolicy.Policy, main modelSelection, keys ...string) (core.Provider, error) {
	return runtime.AuxProviderForPolicy(cfg, policy, main, keys...)
}
