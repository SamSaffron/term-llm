package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRouterManagementKeyIsLazyAndSeparateFromInference(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("OPENROUTER_API_KEY", "inference-key")
	marker := filepath.Join(dir, "management-key-resolved")
	reference := "$(touch " + marker + "; printf management-secret)"
	configDir := filepath.Join(dir, "term-llm")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := fmt.Sprintf("default_provider: openrouter\nproviders:\n  openrouter:\n    management_key: %q\n", reference)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	provider, err := cfg.GetResolvedProviderConfig("openrouter")
	if err != nil || provider == nil {
		t.Fatalf("resolve provider: %v", err)
	}
	if provider.ManagementKey != reference {
		t.Fatal("management credential reference was not preserved")
	}
	if err := provider.ResolveForInference(); err != nil {
		t.Fatal(err)
	}
	if provider.ResolvedAPIKey != "inference-key" {
		t.Fatal("inference must use the regular API key")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("config load and inference must not resolve management_key")
	}
	if !IsKnownKey("providers.openrouter.management_key") {
		t.Fatal("management_key must be registered in config schema")
	}
	for _, field := range ProviderKeySpecs() {
		if field.Path == "management_key" {
			if !field.Sensitive {
				t.Fatal("management_key must be sensitive")
			}
			return
		}
	}
	t.Fatal("management_key missing from provider field specs")
}
