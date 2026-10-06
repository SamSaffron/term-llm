package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func clearProviderDiscoveryEnv(t *testing.T) {
	t.Helper()
	for _, spec := range BuiltinProviders() {
		for _, v := range spec.EnablingEnv() {
			t.Setenv(v, "")
		}
	}
}

func TestProviderDiscoveryModeAndValidation(t *testing.T) {
	if got, ok := DefaultForKey("provider_discovery"); !ok || got != ProviderDiscoveryAuto {
		t.Fatalf("provider_discovery default = %#v, %v", got, ok)
	}
	var nilCfg *Config
	if got := nilCfg.ProviderDiscoveryMode(); got != ProviderDiscoveryAuto {
		t.Fatalf("nil config mode = %q, want auto", got)
	}
	for value, want := range map[string]string{"": "auto", "auto": "auto", " Config ": "config", "ENV": "env"} {
		cfg := &Config{ProviderDiscovery: value}
		if got := cfg.ProviderDiscoveryMode(); got != want {
			t.Errorf("ProviderDiscoveryMode(%q) = %q, want %q", value, got, want)
		}
		if err := cfg.ValidateProviderDiscovery(); err != nil {
			t.Errorf("ValidateProviderDiscovery(%q) = %v", value, err)
		}
	}
	err := (&Config{ProviderDiscovery: "strict"}).ValidateProviderDiscovery()
	if err == nil || !strings.Contains(err.Error(), `invalid provider_discovery "strict"`) {
		t.Fatalf("ValidateProviderDiscovery(strict) = %v", err)
	}
}

func TestProviderConfiguredViaByDiscoveryMode(t *testing.T) {
	clearProviderDiscoveryEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test")

	disabled := false
	providers := map[string]ProviderConfig{
		"openai":    {Model: "gpt-5"},
		"anthropic": {Model: "claude-sonnet-4-6", FromDefaults: true},
		"copilot":   {Enabled: &disabled},
		"corp-llm":  {Type: ProviderTypeOpenAICompat},
	}
	cases := map[string]map[string]string{
		ProviderDiscoveryAuto: {
			"openai": ConfiguredViaConfig, "corp-llm": ConfiguredViaConfig,
			"zen": ConfiguredViaDefault, "xai": ConfiguredViaEnv, "claude-bin": ConfiguredViaLogin,
			"anthropic": "", "copilot": "",
		},
		ProviderDiscoveryEnv: {
			"openai": ConfiguredViaConfig, "corp-llm": ConfiguredViaConfig,
			"zen": "", "xai": ConfiguredViaEnv, "claude-bin": "",
			"anthropic": "", "copilot": "",
		},
		ProviderDiscoveryConfig: {
			"openai": ConfiguredViaConfig, "corp-llm": ConfiguredViaConfig,
			"zen": "", "xai": "", "claude-bin": "",
			"anthropic": "", "copilot": "",
		},
	}
	for mode, want := range cases {
		cfg := &Config{DefaultProvider: "zen", ProviderDiscovery: mode, Providers: providers}
		probes := 0
		loggedIn := func(name string) bool {
			probes++
			return name == "claude-bin" || name == "copilot"
		}
		for name, wantVia := range want {
			if got := cfg.ProviderConfiguredVia(name, loggedIn); got != wantVia {
				t.Errorf("%s: ProviderConfiguredVia(%q) = %q, want %q", mode, name, got, wantVia)
			}
		}
		if mode != ProviderDiscoveryAuto && probes != 0 {
			t.Errorf("%s: login detector called %d times, want none", mode, probes)
		}
	}
}

func TestProviderNotEnabledError(t *testing.T) {
	clearProviderDiscoveryEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test")
	providers := map[string]ProviderConfig{
		"anthropic": {},
		"openai":    {Model: "gpt-5", FromDefaults: true},
	}

	for _, tc := range []struct {
		mode, name string
		wantErr    string
	}{
		{ProviderDiscoveryAuto, "chatgpt", ""},
		{ProviderDiscoveryAuto, "openai", ""},
		{ProviderDiscoveryConfig, "anthropic", ""},
		{ProviderDiscoveryConfig, "debug", ""},
		{ProviderDiscoveryConfig, "xai", `provider "xai" is not enabled: provider_discovery is "config" and config.yaml has no providers.xai block (add "xai: {}" under providers: to enable it)`},
		{ProviderDiscoveryConfig, "openai", `config.yaml has no providers.openai block`},
		{ProviderDiscoveryConfig, "chatgpt", `config.yaml has no providers.chatgpt block`},
		{ProviderDiscoveryEnv, "xai", ""},
		{ProviderDiscoveryEnv, "anthropic", ""},
		{ProviderDiscoveryEnv, "openai", `neither providers.openai in config.yaml nor $OPENAI_API_KEY is set`},
		{ProviderDiscoveryEnv, "chatgpt", `config.yaml has no providers.chatgpt block`},
	} {
		cfg := &Config{DefaultProvider: tc.name, ProviderDiscovery: tc.mode, Providers: providers}
		err := cfg.ProviderNotEnabledError(tc.name)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s/%s: unexpected error %v", tc.mode, tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s/%s: error = %v, want containing %q", tc.mode, tc.name, err, tc.wantErr)
		}
	}
}

func TestApplyOverridesDoesNotDeclareProvider(t *testing.T) {
	cfg := &Config{ProviderDiscovery: ProviderDiscoveryConfig, Providers: map[string]ProviderConfig{}}
	cfg.ApplyOverrides("xai", "grok-4")
	if cfg.Providers["xai"].Model != "grok-4" {
		t.Fatalf("model override not applied: %#v", cfg.Providers["xai"])
	}
	if cfg.ProviderDeclared("xai") {
		t.Fatal("a -p model override must not make xai look declared in config.yaml")
	}
	if err := cfg.ProviderNotEnabledError("xai"); err == nil {
		t.Fatal("xai enabled by a runtime override under provider_discovery: config")
	}
}

func TestLoad_ProviderDiscoveryConfigDeclaredBlocks(t *testing.T) {
	viper.Reset()
	defer viper.Reset()
	clearProviderDiscoveryEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test")

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	configYAML := `provider_discovery: config
default_provider: anthropic
providers:
  anthropic: {}
  chatgpt:
  claude-bin: {}
  ollama: {}
  grok-bin:
    enabled: true
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.ProviderDiscoveryMode(); got != ProviderDiscoveryConfig {
		t.Fatalf("mode = %q, want config", got)
	}
	for name, want := range map[string]bool{"anthropic": true, "chatgpt": true, "claude-bin": true, "ollama": true, "grok-bin": true, "openai": false, "xai": false} {
		if got := cfg.ProviderConfiguredVia(name, nil) == ConfiguredViaConfig; got != want {
			t.Errorf("provider %q enabled = %v, want %v", name, got, want)
		}
	}
}

func TestLoad_RejectsInvalidProviderDiscovery(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("provider_discovery: strict\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "provider_discovery") {
		t.Fatalf("Load error = %v, want invalid provider_discovery", err)
	}
}
