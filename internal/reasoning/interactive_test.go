package reasoning

import (
	"github.com/samsaffron/term-llm/internal/config"
	"testing"
)

func TestInteractiveSourceAllowsIgnoresDisplay(t *testing.T) {
	for _, source := range []string{"summary_only", "summary_or_provider_safe", "all"} {
		for _, display := range []string{"off", "status", "collapsed", "expanded", "raw"} {
			cfg := config.DefaultReasoningConfig()
			cfg.Source = source
			cfg.Display = display
			for _, kind := range []string{KindSummary, KindRaw, KindEncrypted, KindUnknown, ""} {
				want := kind == KindSummary || (kind == KindRaw && source != "summary_only")
				if got := InteractiveSourceAllows(kind, cfg); got != want {
					t.Errorf("source=%s display=%s kind=%s: %t != %t", source, display, kind, got, want)
				}
			}
		}
	}
}
