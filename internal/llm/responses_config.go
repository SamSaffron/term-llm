package llm

import "github.com/samsaffron/term-llm/internal/config"

func responsesOptionsFromConfig(cfg config.ResponsesConfig) ResponsesOptions {
	return ResponsesOptions{
		ReasoningMode:    cfg.ReasoningMode,
		ReasoningContext: cfg.ReasoningContext,
		MultiAgent: MultiAgentOptions{
			Enabled:                cfg.MultiAgent.Enabled,
			EnabledSet:             cfg.MultiAgent.Enabled,
			MaxConcurrentSubagents: cfg.MultiAgent.MaxConcurrentSubagents,
		},
		ProgrammaticToolCalling: ProgrammaticToolCallingOptions{
			Enabled:    cfg.ProgrammaticToolCalling.Enabled,
			EnabledSet: cfg.ProgrammaticToolCalling.Enabled,
			Tools:      append([]string(nil), cfg.ProgrammaticToolCalling.Tools...),
		},
		PromptCache: PromptCacheOptions{
			Mode: cfg.PromptCache.Mode,
			TTL:  cfg.PromptCache.TTL,
		},
	}
}
