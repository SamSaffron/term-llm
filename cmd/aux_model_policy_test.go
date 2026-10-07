package cmd

import (
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	"testing"
)

func TestAuxProviderFallsBackToAllowedMainModel(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "allowed", FastModel: "denied"}}}
	p, err := auxProviderForPolicy(cfg, modelpolicy.Policy{}.With("boss", []string{"debug:allowed"}), modelSelection{Provider: "debug", Model: "allowed"}, "debug")
	if err != nil || p == nil {
		t.Fatalf("allowed main fallback provider=%v err=%v", p, err)
	}
}
