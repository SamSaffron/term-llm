package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestServeEnableFileTrackingFlag(t *testing.T) {
	flag := serveCmd.Flags().Lookup("enable-file-tracking")
	if flag == nil {
		t.Fatal("serve --enable-file-tracking flag is not registered")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--enable-file-tracking default = %q, want false", flag.DefValue)
	}
}

func TestApplyServeFileTrackingOverride(t *testing.T) {
	t.Run("enabled overrides disabled config", func(t *testing.T) {
		cfg := &config.Config{}
		applyServeFileTrackingOverride(cfg, true)
		if !cfg.FileTracking.Enabled {
			t.Fatal("serve flag did not enable file tracking")
		}
	})

	t.Run("omitted preserves config", func(t *testing.T) {
		for _, configured := range []bool{false, true} {
			cfg := &config.Config{FileTracking: config.FileTrackingConfig{Enabled: configured}}
			applyServeFileTrackingOverride(cfg, false)
			if cfg.FileTracking.Enabled != configured {
				t.Fatalf("configured enabled = %t, got %t", configured, cfg.FileTracking.Enabled)
			}
		}
	})
}
