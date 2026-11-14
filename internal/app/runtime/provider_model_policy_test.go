package runtime

import (
	"github.com/term-llm/term-llm/internal/agents"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/modelpolicy"
	runpkg "github.com/term-llm/term-llm/internal/run"
	"strings"
	"testing"
)

func TestProviderAllowedModelsSelectionAdmission(t *testing.T) {
	for _, in := range []ModelSelectionInput{
		{},
		{ProviderFlag: "debug:denied"},
		{ModelOverride: "debug:denied"},
		{Agent: &agents.Agent{Name: "agent", Provider: "debug", Model: "denied", AllowedModels: []string{"debug:*"}}},
		{Parent: runpkg.ParentModel{Provider: "debug", Model: "denied"}},
		{ResumeModel: runpkg.ParentModel{Provider: "debug", Model: "denied"}},
		{Fast: true},
	} {
		cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "denied", FastModel: "denied", AllowedModels: []string{"allowed"}}}}
		if _, _, err := SelectRunModel(cfg, in); err == nil || !strings.Contains(err.Error(), "providers.debug.allowed_models") {
			t.Fatalf("selection bypassed provider ceiling: %+v err=%v", in, err)
		}
	}
}

func TestProviderAllowedModelsAuxFallback(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "allowed", FastModel: "denied", AllowedModels: []string{"allowed"}}}}
	p, err := AuxProviderForPolicy(cfg, modelpolicy.Policy{}, ModelSelection{Provider: "debug", Model: "allowed"}, "debug")
	if err != nil || p == nil {
		t.Fatalf("allowed main fallback=%v err=%v", p, err)
	}
}

func TestProviderAllowedModelsConfigCloneIsIndependent(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"p": {AllowedModels: []string{"allowed"}}, "empty": {AllowedModels: []string{}}}}
	clone := CloneConfig(cfg)
	clone.Providers["p"].AllowedModels[0] = "changed"
	if cfg.Providers["p"].AllowedModels[0] != "allowed" {
		t.Fatal("run-local copy mutated operator policy")
	}
	if clone.Providers["empty"].AllowedModels == nil {
		t.Fatal("clone lost explicit empty policy")
	}
}
