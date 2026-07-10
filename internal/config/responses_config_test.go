package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

func TestLoadProviderAdvancedResponsesConfig(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("OPENAI_API_KEY", "test-key")
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	configYAML := `default_provider: openai
providers:
  openai:
    model: gpt-5.6-sol
    responses:
      reasoning_mode: pro
      reasoning_context: all_turns
      multi_agent:
        enabled: true
        max_concurrent_subagents: 4
      programmatic_tool_calling:
        enabled: true
        tools: [read_file, grep]
      prompt_cache:
        mode: explicit
        ttl: 30m
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	responses := cfg.Providers["openai"].Responses
	if responses.ReasoningMode != "pro" || responses.ReasoningContext != "all_turns" {
		t.Fatalf("reasoning responses config = %+v", responses)
	}
	if !responses.MultiAgent.Enabled || responses.MultiAgent.MaxConcurrentSubagents != 4 {
		t.Fatalf("multi_agent = %+v", responses.MultiAgent)
	}
	if !responses.ProgrammaticToolCalling.Enabled || len(responses.ProgrammaticToolCalling.Tools) != 2 {
		t.Fatalf("programmatic_tool_calling = %+v", responses.ProgrammaticToolCalling)
	}
	if responses.PromptCache.Mode != "explicit" || responses.PromptCache.TTL != "30m" {
		t.Fatalf("prompt_cache = %+v", responses.PromptCache)
	}
	for _, key := range []string{
		"providers.openai.responses.reasoning_mode",
		"providers.openai.responses.reasoning_context",
		"providers.openai.responses.multi_agent.enabled",
		"providers.openai.responses.programmatic_tool_calling.tools",
		"providers.openai.responses.prompt_cache.ttl",
	} {
		if !IsKnownKey(key) {
			t.Fatalf("IsKnownKey(%q) = false", key)
		}
	}
}
