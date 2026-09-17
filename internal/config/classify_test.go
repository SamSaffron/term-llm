package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestClassifyProviderConfigDefaultsAndSensitiveKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	viper.Reset()
	t.Cleanup(viper.Reset)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Classify.Providers["typesafe"].Model != DefaultTypeSafeModel || cfg.Classify.Providers["typesafe"].BaseURL != DefaultTypeSafeBaseURL || cfg.Classify.Providers["typesafe"].TimeoutSeconds != DefaultTypeSafeTimeoutSeconds {
		t.Fatalf("unexpected TypeSafe defaults: %#v", cfg.Classify.Providers["typesafe"])
	}

	defaults := GetDefaults()
	for key, want := range map[string]any{
		"classify.providers.typesafe.model":           DefaultTypeSafeModel,
		"classify.providers.typesafe.base_url":        DefaultTypeSafeBaseURL,
		"classify.providers.typesafe.timeout_seconds": DefaultTypeSafeTimeoutSeconds,
	} {
		if got := defaults[key]; got != want {
			t.Fatalf("%s default = %#v, want %#v", key, got, want)
		}
	}
	if _, ok := defaults["classify.providers.typesafe.api_key"]; ok {
		t.Fatal("classify.providers.typesafe.api_key should not have a default")
	}

	foundSensitive := false
	for _, spec := range ConfigKeySpecs() {
		if spec.Path == "classify.providers.typesafe.api_key" {
			foundSensitive = spec.Sensitive
			break
		}
	}
	if !foundSensitive {
		t.Fatal("classify.providers.typesafe.api_key is not registered as sensitive")
	}
}

func TestTypeSafeKeyUsesEnvFallback(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "env-typesafe-key")

	ref := (ClassifyProviderConfig{}).Key()
	if !ref.Configured() {
		t.Fatal("TypeSafe key should be configured by TYPESAFE_API_KEY")
	}
	got, err := ref.Resolve()
	if err != nil || got != "env-typesafe-key" {
		t.Fatalf("TypeSafe key = %q (err %v), want env-typesafe-key", got, err)
	}

	got, err = (ClassifyProviderConfig{APIKey: "configured-key"}).Key().Resolve()
	if err != nil || got != "configured-key" {
		t.Fatalf("configured TypeSafe key = %q (err %v), want configured-key", got, err)
	}
}

func TestLoadConfigDoesNotResolveTypeSafeCredential(t *testing.T) {
	ResetCredentialCache()
	t.Cleanup(ResetCredentialCache)

	configHome := t.TempDir()
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(configHome, "resolved")
	contents := "classify:\n  providers:\n    typesafe:\n      api_key: \"$(touch " + marker + "; printf typesafe-key)\"\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("XDG_CONFIG_HOME", configHome)
	viper.Reset()
	t.Cleanup(viper.Reset)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg.Classify.Providers["typesafe"].APIKey, "$(") {
		t.Fatalf("test config was not loaded: classify.providers.typesafe.api_key = %q", cfg.Classify.Providers["typesafe"].APIKey)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("loading config must not resolve TypeSafe credentials")
	}

	got, err := cfg.Classify.Providers["typesafe"].Key().Resolve()
	if err != nil || got != "typesafe-key" {
		t.Fatalf("TypeSafe key = %q (err %v)", got, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("TypeSafe key resolution should have run the command: %v", err)
	}
}

func TestClassifyProviderSelection(t *testing.T) {
	c := ClassifyConfig{DefaultProvider: "local", Providers: map[string]ClassifyProviderConfig{
		"local":       {Type: "typesafe", APIKey: "$(exit 1)", Model: "custom"},
		"unsupported": {Type: "other"}, "untyped": {},
	}}
	for _, name := range []string{"", "local", "typesafe"} {
		p, err := c.ResolveProvider(name)
		if err != nil {
			t.Fatal(err)
		}
		if p.Type != "typesafe" || p.BaseURL != DefaultTypeSafeBaseURL || p.TimeoutSeconds != 10 {
			t.Fatalf("provider: %#v", p)
		}
		if name != "typesafe" && p.Model != "custom" {
			t.Fatalf("model: %s", p.Model)
		}
	}
	for _, name := range []string{"missing", "unsupported", "untyped"} {
		if _, err := c.ResolveProvider(name); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestClassifyAliasSchema(t *testing.T) {
	for _, field := range []string{"type", "api_key", "model", "base_url", "timeout_seconds"} {
		if !IsKnownKey("classify.providers.custom." + field) {
			t.Fatalf("unknown field %s", field)
		}
	}
	for _, key := range []string{"classify.providers.custom.typo", "classify.providers.custom.model.extra", "classify.providers..api_key"} {
		if IsKnownKey(key) {
			t.Fatalf("accepted invalid key %s", key)
		}
	}
	specs := ClassifyKeySpecs([]string{"custom"})
	if len(specs) != 5 {
		t.Fatalf("specs: %v", specs)
	}
	for _, spec := range specs {
		if strings.HasSuffix(spec.Path, ".api_key") && !spec.Sensitive {
			t.Fatal("alias key not sensitive")
		}
	}
}
