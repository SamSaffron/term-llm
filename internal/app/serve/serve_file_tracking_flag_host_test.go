package serve

import (
	"testing"

	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestApplyServeFileTrackingOverride(t *testing.T) {
	t.Run("enabled overrides disabled config", func(t *testing.T) {
		cfg := &config.Config{}
		appruntime.ApplyServeFileTrackingOverride(cfg, true)
		if !cfg.FileTracking.Enabled {
			t.Fatal("serve flag did not enable file tracking")
		}
	})

	t.Run("omitted preserves config", func(t *testing.T) {
		for _, configured := range []bool{false, true} {
			cfg := &config.Config{FileTracking: config.FileTrackingConfig{Enabled: configured}}
			appruntime.ApplyServeFileTrackingOverride(cfg, false)
			if cfg.FileTracking.Enabled != configured {
				t.Fatalf("configured enabled = %t, got %t", configured, cfg.FileTracking.Enabled)
			}
		}
	})
}
