package config

import (
	"math"
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

	// The default endpoint still inherits the shared credential.
	if ref := (ClassifyProviderConfig{BaseURL: DefaultTypeSafeBaseURL}).Key(); !ref.Configured() {
		t.Fatal("default TypeSafe base URL should still use TYPESAFE_API_KEY")
	}
}

// TestTypeSafeKeyDoesNotLeakToForeignBaseURL pins that redirecting a classify
// provider to another host cannot silently reuse the primary TypeSafe key.
func TestTypeSafeKeyDoesNotLeakToForeignBaseURL(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "env-typesafe-key")

	foreign := ClassifyProviderConfig{BaseURL: "https://classify.example"}
	if foreign.Key().Configured() {
		t.Fatal("provider on a non-default base URL must not inherit TYPESAFE_API_KEY")
	}
	if got, err := foreign.Key().Resolve(); err == nil && got == "env-typesafe-key" {
		t.Fatal("TYPESAFE_API_KEY leaked to a non-default classify endpoint")
	}

	// An explicit key on the same provider is still honored.
	explicit := ClassifyProviderConfig{BaseURL: "https://classify.example", APIKey: "scoped-key"}
	if got, err := explicit.Key().Resolve(); err != nil || got != "scoped-key" {
		t.Fatalf("scoped key = %q (err %v), want scoped-key", got, err)
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

func TestGuardianClassifyConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	viper.Reset()
	t.Cleanup(viper.Reset)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Guardian.Backend != "llm" || cfg.Guardian.Classify.MinConfidence != 0.15 {
		t.Fatalf("defaults: %+v", cfg.Guardian)
	}
	for _, value := range []float64{0, 0.25, 1, -0.1, 1.1} {
		viper.Set("guardian.backend", "classify")
		viper.Set("guardian.classify.provider", "alias")
		viper.Set("guardian.classify.min_confidence", value)
		cfg, err = Load()
		if value < 0 || value > 1 {
			if err == nil {
				t.Fatalf("accepted %g", value)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Guardian.Classify.MinConfidence != value || cfg.Guardian.Classify.Provider != "alias" {
			t.Fatalf("%+v", cfg.Guardian)
		}
	}
}

func TestGuardianClassifyConfidenceRejectsNonFinite(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := (GuardianClassifyConfig{MinConfidence: value}).Validate(); err == nil {
			t.Fatalf("accepted %g", value)
		}
	}
}
