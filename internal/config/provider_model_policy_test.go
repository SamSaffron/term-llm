package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestProviderAllowedModelsLoadAndSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeTestConfig(t, home, `default_provider: claude-bin
providers:
  claude-bin:
    model: claude-opus-5-5
    fast_model: claude-sonnet-5-5
    allowed_models: [claude-opus-5-5, claude-sonnet-5-5]
  local:
    type: ollama
    allowed_models: [qwen:8b]
`)
	cfg, err := loadUnresolved()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude-opus-5-5", "claude-sonnet-5-5"}
	if !reflect.DeepEqual(cfg.Providers["claude-bin"].AllowedModels, want) {
		t.Fatalf("loaded allowlist: %v", cfg.Providers["claude-bin"].AllowedModels)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := loadUnresolved()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Providers["claude-bin"].AllowedModels, want) {
		t.Fatalf("saved allowlist: %v", got.Providers["claude-bin"].AllowedModels)
	}
	if !reflect.DeepEqual(got.Providers["local"].AllowedModels, []string{"qwen:8b"}) {
		t.Fatalf("colon model ID: %v", got.Providers["local"].AllowedModels)
	}
	if !IsKnownKey("providers.claude-bin.allowed_models") {
		t.Fatal("provider schema does not expose allowed_models")
	}
}

func TestProviderAllowedModelsLoadRejectsMalformedEntries(t *testing.T) {
	for _, value := range []string{`[""]`, `[" claude-opus-5-5"]`, `["claude opus"]`, `["claude-*"]`, `["*"]`} {
		t.Run(value, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			writeTestConfig(t, home, "providers:\n  claude-bin:\n    allowed_models: "+value+"\n")
			if _, err := loadUnresolved(); err == nil || !strings.Contains(err.Error(), "providers.claude-bin.allowed_models") {
				t.Fatalf("expected useful validation error, got %v", err)
			}
		})
	}
}

func TestProviderAllowedModelsEmptyIsUnrestricted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeTestConfig(t, home, "providers:\n  claude-bin:\n    allowed_models: []\n")
	cfg, err := loadUnresolved()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers["claude-bin"].AllowedModels) != 0 {
		t.Fatal("empty allowlist must remain unrestricted")
	}
}

func TestProviderAllowedModelsSavePreservesOperatorPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeTestConfig(t, home, "providers:\n  claude-bin:\n    allowed_models: [claude-opus-5-5]\n  custom-removed:\n    type: openai_compatible\n    base_url: https://example.invalid/v1\n    allowed_models: [custom-model]\n")
	if err := Save(&Config{DefaultProvider: "openai", Providers: map[string]ProviderConfig{"claude-bin": {Model: "claude-opus-5-5"}}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadUnresolved()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Providers["claude-bin"].AllowedModels, []string{"claude-opus-5-5"}) {
		t.Fatal("setup erased operator model restriction")
	}
	if _, resurrected := cfg.Providers["custom-removed"]; resurrected {
		t.Fatal("save resurrected a removed custom provider as an incomplete stub")
	}
	cfg.Providers["claude-bin"] = ProviderConfig{AllowedModels: []string{}}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadUnresolved()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers["claude-bin"].AllowedModels) != 0 {
		t.Fatal("explicit empty list did not clear model restriction")
	}
}
