package llm

import "testing"

func TestProviderModelsIncludeGPT54MiniAndNano(t *testing.T) {
	if !containsModelName(ProviderModels["openai"], "gpt-5.4-mini") {
		t.Fatalf("openai models missing gpt-5.4-mini")
	}
	if !containsModelName(ProviderModels["openai"], "gpt-5.4-nano") {
		t.Fatalf("openai models missing gpt-5.4-nano")
	}
	if !containsModelName(ProviderModels["chatgpt"], "gpt-5.4-mini") {
		t.Fatalf("chatgpt models missing gpt-5.4-mini")
	}
	if containsModelName(ProviderModels["chatgpt"], "gpt-5.4-nano") {
		t.Fatalf("chatgpt models unexpectedly include gpt-5.4-nano")
	}
}

func TestProviderFastModelsUseLatestGPT54LightweightModels(t *testing.T) {
	if got := ProviderFastModels["openai"]; got != "gpt-5.4-nano" {
		t.Fatalf("ProviderFastModels[openai] = %q, want %q", got, "gpt-5.4-nano")
	}
	if got := ProviderFastModels["chatgpt"]; got != "gpt-5.4-mini" {
		t.Fatalf("ProviderFastModels[chatgpt] = %q, want %q", got, "gpt-5.4-mini")
	}
}

func containsModelName(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
