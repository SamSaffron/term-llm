package modelpolicy

import (
	"github.com/samsaffron/term-llm/internal/config"
	"strings"
	"testing"
)

func TestProviderAndAgentAllowedModelsIntersect(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"debug": {AllowedModels: []string{"provider-only", "shared"}}}}
	for _, policy := range []Policy{{}, (Policy{}).With("agent", []string{"debug:*"}), (Policy{}).With("parent", []string{"debug:*"}).With("child", []string{"debug:*"})} {
		if err := policy.CheckWithConfig(cfg, "child", "debug", "denied"); err == nil || !strings.Contains(err.Error(), "providers.debug.allowed_models") {
			t.Fatalf("provider ceiling bypassed: %v", err)
		}
	}
	p := Policy(Policy{}).With("agent", []string{"debug:agent-only", "debug:shared"})
	if err := p.CheckWithConfig(cfg, "agent", "debug", "shared"); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"agent-only", "provider-only"} {
		if err := p.CheckWithConfig(cfg, "agent", "debug", model); err == nil {
			t.Fatalf("intersection allowed %q", model)
		}
	}
}
