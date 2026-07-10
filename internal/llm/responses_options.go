package llm

import (
	"fmt"
	"strings"
)

func mergeResponsesOptions(defaults ResponsesOptions, override *ResponsesOptions, ephemeral bool) ResponsesOptions {
	if ephemeral && override == nil {
		return ResponsesOptions{}
	}
	out := ResponsesOptions{}
	if !ephemeral {
		out = defaults
	}
	if override == nil {
		return out
	}
	if v := strings.TrimSpace(override.ReasoningMode); v != "" {
		out.ReasoningMode = v
	}
	if v := strings.TrimSpace(override.ReasoningContext); v != "" {
		out.ReasoningContext = v
	}
	if override.MultiAgent.EnabledSet || override.MultiAgent.Enabled {
		out.MultiAgent.Enabled = override.MultiAgent.Enabled
		out.MultiAgent.EnabledSet = true
	}
	if override.MultiAgent.MaxConcurrentSubagents != 0 {
		out.MultiAgent.MaxConcurrentSubagents = override.MultiAgent.MaxConcurrentSubagents
	}
	if override.ProgrammaticToolCalling.EnabledSet || override.ProgrammaticToolCalling.Enabled {
		out.ProgrammaticToolCalling.Enabled = override.ProgrammaticToolCalling.Enabled
		out.ProgrammaticToolCalling.EnabledSet = true
	}
	if override.ProgrammaticToolCalling.Tools != nil {
		out.ProgrammaticToolCalling.Tools = append([]string(nil), override.ProgrammaticToolCalling.Tools...)
	}
	if v := strings.TrimSpace(override.PromptCache.Mode); v != "" {
		out.PromptCache.Mode = v
	}
	if v := strings.TrimSpace(override.PromptCache.TTL); v != "" {
		out.PromptCache.TTL = v
	}
	return out
}

func validateResponsesOptions(provider, model string, opts *ResponsesOptions, tools []ToolSpec) (ResponsesOptions, error) {
	if opts == nil {
		return ResponsesOptions{}, nil
	}
	normalized := *opts
	normalized.ReasoningMode = strings.ToLower(strings.TrimSpace(normalized.ReasoningMode))
	normalized.ReasoningContext = strings.ToLower(strings.TrimSpace(normalized.ReasoningContext))
	normalized.PromptCache.Mode = strings.ToLower(strings.TrimSpace(normalized.PromptCache.Mode))
	normalized.PromptCache.TTL = strings.ToLower(strings.TrimSpace(normalized.PromptCache.TTL))

	if normalized.ReasoningMode != "" && normalized.ReasoningMode != "standard" && normalized.ReasoningMode != "pro" {
		return ResponsesOptions{}, fmt.Errorf("unsupported reasoning mode %q (want standard or pro)", normalized.ReasoningMode)
	}
	if normalized.ReasoningContext != "" && normalized.ReasoningContext != "auto" && normalized.ReasoningContext != "current_turn" && normalized.ReasoningContext != "all_turns" {
		return ResponsesOptions{}, fmt.Errorf("unsupported reasoning context %q", normalized.ReasoningContext)
	}
	if normalized.MultiAgent.Enabled && normalized.MultiAgent.MaxConcurrentSubagents == 0 {
		normalized.MultiAgent.MaxConcurrentSubagents = 3
	}
	if normalized.MultiAgent.MaxConcurrentSubagents < 0 {
		return ResponsesOptions{}, fmt.Errorf("multi-agent max_concurrent_subagents must be positive")
	}
	if normalized.ProgrammaticToolCalling.Enabled && len(normalized.ProgrammaticToolCalling.Tools) == 0 {
		return ResponsesOptions{}, fmt.Errorf("programmatic tool calling requires at least one eligible tool")
	}
	if normalized.PromptCache.Mode != "" && normalized.PromptCache.Mode != "implicit" && normalized.PromptCache.Mode != "explicit" {
		return ResponsesOptions{}, fmt.Errorf("unsupported prompt cache mode %q", normalized.PromptCache.Mode)
	}
	if normalized.PromptCache.TTL != "" && normalized.PromptCache.TTL != "30m" {
		return ResponsesOptions{}, fmt.Errorf("unsupported prompt cache TTL %q (only 30m is supported)", normalized.PromptCache.TTL)
	}

	advanced := normalized.ReasoningMode != "" || normalized.ReasoningContext != "" ||
		normalized.MultiAgent.Enabled || normalized.MultiAgent.MaxConcurrentSubagents != 0 ||
		normalized.ProgrammaticToolCalling.Enabled || len(normalized.ProgrammaticToolCalling.Tools) != 0 ||
		normalized.PromptCache.Mode != "" || normalized.PromptCache.TTL != ""
	if advanced && !SupportsReasoningMode(provider, model) {
		return ResponsesOptions{}, fmt.Errorf("advanced Responses controls are supported only by OpenAI API GPT-5.6 models (provider=%s model=%s)", provider, model)
	}
	if normalized.ProgrammaticToolCalling.Enabled {
		available := make(map[string]bool, len(tools))
		for _, tool := range tools {
			available[tool.Name] = true
		}
		for _, name := range normalized.ProgrammaticToolCalling.Tools {
			if !available[name] {
				return ResponsesOptions{}, fmt.Errorf("programmatic tool %q is not present in the request", name)
			}
		}
	}
	return normalized, nil
}

func parseModelEffortForProvider(provider, model string) (string, string) {
	if strings.TrimSpace(model) == "" {
		return model, ""
	}
	base, effort := BaseModelAndEffortForProvider(provider, model)
	return base, effort
}

func composeBetaHeader(existing, addition string) string {
	addition = strings.TrimSpace(addition)
	if addition == "" {
		return strings.TrimSpace(existing)
	}
	for _, value := range strings.Split(existing, ",") {
		if strings.TrimSpace(value) == addition {
			return strings.TrimSpace(existing)
		}
	}
	if strings.TrimSpace(existing) == "" {
		return addition
	}
	return strings.TrimSpace(existing) + "," + addition
}
