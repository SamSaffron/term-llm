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

func vLLMThinkingSettings(model, effort, paramOverride string) (map[string]interface{}, int) {
	key := vLLMThinkingParam(model, paramOverride)
	if key == "thinking" {
		return vLLMDeepSeekThinkingSettings(effort), 0
	}
	enableThinking, budget := vLLMQwenThinkingSettings(effort)
	return map[string]interface{}{key: enableThinking}, budget
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

func vLLMDeepSeekThinkingSettings(effort string) map[string]interface{} {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "", "default", "minimal", "none", "off", "false":
		return map[string]interface{}{"thinking": false}
	case "max", "xhigh":
		return map[string]interface{}{"thinking": true, "reasoning_effort": "max"}
	default:
		// DeepSeek/vLLM exposes only Think High and Think Max. DeepSeek's
		// official API maps low and medium to high for compatibility, and vLLM's
		// DeepSeek V4 tokenizer does the same for any non-max effort.
		return map[string]interface{}{"thinking": true, "reasoning_effort": "high"}
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
