package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

func TestOpenRouterFreeDefaultsPreserveSavedChoices(t *testing.T) {
	for _, tc := range []struct{ name, saved, provider, model, fast string }{
		{"new config", "", "openrouter", "openrouter/free", "openrouter/free"},
		{"saved provider", "default_provider: zen\nproviders:\n  zen:\n    model: saved-model\n    fast_model: saved-fast\n", "zen", "saved-model", "saved-fast"},
		{"saved OpenRouter models", "default_provider: openrouter\nproviders:\n  openrouter:\n    model: saved-model\n    fast_model: saved-fast\n", "openrouter", "saved-model", "saved-fast"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("OPENROUTER_API_KEY", "test-key")
			viper.Reset()
			t.Cleanup(viper.Reset)
			if tc.saved != "" {
				dir, err := GetConfigDir()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(tc.saved), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			p := cfg.Providers[cfg.DefaultProvider]
			if cfg.DefaultProvider != tc.provider || p.Model != tc.model || p.FastModel != tc.fast {
				t.Fatalf("got %s:%s (fast %s), want %s:%s (fast %s)", cfg.DefaultProvider, p.Model, p.FastModel, tc.provider, tc.model, tc.fast)
			}
		})
	}
}
