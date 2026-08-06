package llm

import "strings"

// VLLMProvider implements Provider for vLLM's OpenAI-compatible chat API plus
// vLLM/Qwen-specific thinking controls.
type VLLMProvider struct {
	*OpenAICompatProvider
}

func NewVLLMProviderFull(baseURL, chatURL, apiKey, model, name string) *VLLMProvider {
	if strings.TrimSpace(name) == "" {
		name = "vLLM"
	}
	actualModel, effort := ParseModelEffort(model)
	p := NewOpenAICompatProviderFull(baseURL, chatURL, apiKey, actualModel, name, nil)
	p.effort = effort
	p.vllmThinking = true
	return &VLLMProvider{OpenAICompatProvider: p}
}

func NewVLLMProvider(baseURL, apiKey, model, name string) *VLLMProvider {
	return NewVLLMProviderFull(baseURL, "", apiKey, model, name)
}

func vLLMThinkingSettings(model, effort, paramOverride string, configuredEfforts bool) (map[string]interface{}, int, string) {
	key := vLLMThinkingParam(model, paramOverride)
	if key == "thinking" {
		var kwargs map[string]interface{}
		var reasoningEffort string
		if configuredEfforts {
			kwargs, reasoningEffort = vLLMThinkingEffortSettings(effort)
		} else {
			kwargs, reasoningEffort = vLLMLegacyDeepSeekThinkingSettings(effort)
		}
		return kwargs, 0, reasoningEffort
	}
	enableThinking, budget := vLLMQwenThinkingSettings(effort)
	return map[string]interface{}{key: enableThinking}, budget, ""
}

func vLLMThinkingParam(model, override string) string {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "thinking", "think", "deepseek":
		return "thinking"
	case "enable_thinking", "enable-thinking", "qwen":
		return "enable_thinking"
	}
	if isVLLMDeepSeekModel(model) {
		return "thinking"
	}
	return "enable_thinking"
}

func vLLMThinkingEffortSettings(effort string) (map[string]interface{}, string) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	switch effort {
	case "", "default", "none", "off", "false":
		// thinking=false is sufficient to select chat mode. Omit the top-level
		// reasoning_effort rather than redundantly sending "none" as well.
		return map[string]interface{}{"thinking": false}, ""
	default:
		// Explicit model metadata is the capability boundary. Pass enabled
		// efforts through unchanged rather than embedding model-specific mappings.
		return map[string]interface{}{"thinking": true}, effort
	}
}

func vLLMLegacyDeepSeekThinkingSettings(effort string) (map[string]interface{}, string) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "", "default", "minimal", "none", "off", "false":
		return map[string]interface{}{"thinking": false}, ""
	case "max", "xhigh":
		return map[string]interface{}{"thinking": true}, "max"
	default:
		return map[string]interface{}{"thinking": true}, "high"
	}
}

func vLLMQwenThinkingSettings(effort string) (bool, int) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "", "default", "minimal", "none", "off", "false":
		// Do not send thinking_token_budget when thinking is disabled. Recent vLLM
		// requires the server to be started with --reasoning-config before it will
		// accept per-request thinking_token_budget; sending a default budget with
		// enable_thinking=false makes otherwise plain requests fail.
		return false, 0
	case "low":
		return true, 1024
	case "medium":
		return true, 4096
	case "high", "xhigh", "max":
		return true, 10000
	default:
		return true, 0
	}
}

func isVLLMDeepSeekModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(model, "deepseek")
}
