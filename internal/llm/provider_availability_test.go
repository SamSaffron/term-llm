package llm

import (
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, spec := range config.BuiltinProviders() {
		for _, v := range spec.EnablingEnv() {
			t.Setenv(v, "")
		}
	}
}

func TestProviderConfiguredVia(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test")
	t.Setenv("VLLM_API_KEY", "vllm-test") // no default endpoint: not enough for vllm
	t.Setenv("AWS_PROFILE", "work")       // generic AWS state: not enough for bedrock

	enabled, disabled := true, false
	cfg := &config.Config{
		DefaultProvider: "zen",
		Providers: map[string]config.ProviderConfig{
			"openai":    {Model: "gpt-5"},
			"anthropic": {Model: "claude-sonnet-4-6", FromDefaults: true},
			"copilot":   {Enabled: &disabled},
			"corp-llm":  {Type: config.ProviderTypeOpenAICompat},
			"old-llm":   {Type: config.ProviderTypeOpenAICompat, Enabled: &disabled},
			"gemini":    {Enabled: &enabled},
		},
	}
	loggedIn := func(name string) bool { return name == "claude-bin" || name == "copilot" }

	for name, want := range map[string]string{
		"openai":     ConfiguredViaConfig,
		"gemini":     ConfiguredViaConfig,
		"corp-llm":   ConfiguredViaConfig,
		"zen":        ConfiguredViaDefault,
		"xai":        ConfiguredViaEnv,
		"claude-bin": ConfiguredViaLogin,
		"anthropic":  "", // only schema defaults
		"copilot":    "", // signed in but disabled
		"old-llm":    "",
		"vllm":       "",
		"bedrock":    "",
		"grok":       "",
		"unknown":    "",
	} {
		if got := ProviderConfiguredVia(cfg, name, loggedIn); got != want {
			t.Errorf("ProviderConfiguredVia(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestProviderCredentialCache_ReusesProbesUntilTTL(t *testing.T) {
	now := time.Unix(1_000, 0)
	calls := 0
	signedIn := false
	cache := ProviderCredentialCache{
		Now: func() time.Time { return now },
		Detect: func(name string) bool {
			if name == "chatgpt" {
				calls++
			}
			return name == "chatgpt" && signedIn
		},
	}

	if cache.Has("chatgpt") {
		t.Fatal("chatgpt reported signed in before login")
	}
	signedIn = true
	if cache.Has("chatgpt") || calls != 1 {
		t.Fatalf("cache refreshed early: calls = %d", calls)
	}
	now = now.Add(ProviderCredentialCacheTTL)
	if !cache.Has("chatgpt") || calls != 2 {
		t.Fatalf("cache did not refresh after TTL: calls = %d", calls)
	}
}
