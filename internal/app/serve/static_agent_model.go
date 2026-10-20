package serve

import (
	"context"

	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/tools"
)

// wireStaticAgentModelAdmission binds the same selected model and rule chain
// to every agent-launch tool of a one-shot runtime.
func WireStaticAgentModelAdmission(cfg *config.Config, mgr *tools.ToolManager, spawn *SpawnAgentRunner, policy modelpolicy.Policy, selected runtime.ModelSelection) {
	state := func() (modelpolicy.Policy, runpkg.ParentModel) {
		return policy, runpkg.ParentModel{Provider: selected.Provider, Model: selected.Model}
	}
	if spawn != nil {
		spawn.SetModelPolicySource(func() ParentModelState {
			p, parent := state()
			return ParentModelState{Policy: p, Provider: parent.Provider, Model: parent.Model}
		})
	}
	if mgr != nil && mgr.Registry != nil {
		mgr.Registry.SetAgentModelAdmission(state, func(ctx context.Context, name, model string, inherited modelpolicy.Policy, parent runpkg.ParentModel) error {
			return NewRunner(cfg, RunnerOptions{}).CheckAgentModel(ctx, runpkg.Request{AgentName: name, Model: model, ModelPolicy: inherited, ParentModel: parent})
		})
	}
}
