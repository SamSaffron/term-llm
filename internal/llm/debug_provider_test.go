package llm

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestNewProviderPreservesDebugVariantOverride(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "debug",
		Providers: map[string]config.ProviderConfig{
			"debug": {Model: "compaction"},
		},
	}
	provider, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	if got := provider.Name(); got != "debug:compaction" {
		t.Fatalf("provider name = %q, want debug:compaction", got)
	}
}

func TestDebugCompactionModelLimits(t *testing.T) {
	if got := InputLimitForProviderModel("debug", "compaction"); got != 19_000 {
		t.Fatalf("input limit = %d, want 19000", got)
	}
	if got := OutputLimitForModel("compaction"); got != 1_000 {
		t.Fatalf("output limit = %d, want 1000", got)
	}
}
