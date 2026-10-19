package runtime

import (
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	"testing"
)

func TestAuxProviderFallsBackToAllowedMainModel(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "allowed", FastModel: "denied"}}}
	p, err := AuxProviderForPolicy(cfg, modelpolicy.Policy{}.With("boss", []string{"debug:allowed"}), ModelSelection{Provider: "debug", Model: "allowed"}, "debug")
	if err != nil || p == nil {
		t.Fatalf("allowed main fallback provider=%v err=%v", p, err)
	}
}
