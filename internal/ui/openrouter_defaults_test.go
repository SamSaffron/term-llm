package ui

import (
	"testing"

	"github.com/spf13/viper"
)

func TestHeadlessSetupPrefersOpenRouterFree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("PATH", "")
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_API_KEY", "other-key")
	t.Setenv("ZEN_API_KEY", "other-key")
	viper.Reset()
	t.Cleanup(viper.Reset)
	cfg, err := RunHeadlessSetup()
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers[cfg.DefaultProvider]
	if cfg.DefaultProvider != "openrouter" || p.Model != "openrouter/free" || p.FastModel != "openrouter/free" {
		t.Fatalf("got %s:%s (fast %s), want OpenRouter free for both", cfg.DefaultProvider, p.Model, p.FastModel)
	}
	provider, model := wizardGuardianDefault(cfg.DefaultProvider, cfg.Providers)
	if provider != "openrouter" || model != "openrouter/free" {
		t.Fatalf("guardian = %s:%s", provider, model)
	}
}
