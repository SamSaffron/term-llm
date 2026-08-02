package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

func TestGatewayConfigLoadDefaultsValidationAndExplicitProviders(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("TERM_LLM_GATEWAY_TOKEN", "env-token")
	configDir := filepath.Join(dir, "term-llm")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := `default_provider: zen
gateway:
  url: http://gateway:8787
  required: true
  local_providers: [ollama]
providers:
  zen:
    model: explicit-model
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Gateway.Enabled() || !cfg.Gateway.RouteSearch() || !cfg.Gateway.RouteFetch() || cfg.Gateway.TokenEnv != DefaultGatewayTokenEnv {
		t.Fatalf("gateway defaults not loaded: %+v", cfg.Gateway)
	}
	if token, err := cfg.Gateway.ResolveToken(); err != nil || token != "env-token" {
		t.Fatalf("ResolveToken = %q, %v", token, err)
	}
	if !cfg.IsLocalProvider("zen") || !cfg.IsLocalProvider("ollama") {
		t.Fatalf("explicit/local providers not pinned: explicit=%v", cfg.explicitProviders)
	}
	if cfg.IsLocalProvider("openai") {
		t.Fatalf("Viper built-in default was mistaken for explicit local config: %v", cfg.explicitProviders)
	}
}

func TestGatewaySearchFetchExplicitFalseRoundTrips(t *testing.T) {
	value := false
	cfg := GatewayConfig{URL: "https://gateway.example", Search: &value, Fetch: &value}
	if cfg.RouteSearch() || cfg.RouteFetch() {
		t.Fatal("explicit false gateway search/fetch was overridden")
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "search: false") || !strings.Contains(text, "fetch: false") {
		t.Fatalf("explicit false did not serialize: %s", text)
	}
	minimal := GatewayConfig{URL: "https://gateway.example"}
	if !minimal.RouteSearch() || !minimal.RouteFetch() {
		t.Fatal("minimal gateway config did not default search/fetch remote")
	}
}

func TestGatewayTokenResolutionIsDeferred(t *testing.T) {
	cfg := GatewayConfig{URL: "https://gateway.example"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unrelated commands should not resolve gateway token during config load: %v", err)
	}
	if _, err := cfg.ResolveToken(); err == nil {
		t.Fatal("gateway operation accepted missing token")
	}
}

func TestGatewayConfigNoURLPreservesCompatibilityAndRejectsInvalid(t *testing.T) {
	if err := (GatewayConfig{}).Validate(); err != nil {
		t.Fatalf("empty gateway config changed local behavior: %v", err)
	}
	for _, cfg := range []GatewayConfig{
		{Required: true},
		{URL: "file:///tmp/gateway", Token: "x"},
		{URL: "http://gateway", Token: "x", CatalogTTL: "never"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid gateway config accepted: %+v", cfg)
		}
	}
}

func TestGatewayTokenFilePrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(" file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CUSTOM_GATEWAY_TOKEN", "env-token")
	cfg := GatewayConfig{URL: "https://gateway.example", TokenFile: path, TokenEnv: "CUSTOM_GATEWAY_TOKEN"}
	if token, err := cfg.ResolveToken(); err != nil || strings.TrimSpace(token) != "file-token" {
		t.Fatalf("ResolveToken = %q, %v", token, err)
	}
	cfg.Token = "explicit"
	if token, _ := cfg.ResolveToken(); token != "explicit" {
		t.Fatalf("explicit token did not win: %q", token)
	}
}
