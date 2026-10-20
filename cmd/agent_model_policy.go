package cmd

import (
	"context"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// Compatibility aliases until runner and serve command implementations move.
type modelSelection = runtime.ModelSelection
type modelSelectionInput = runtime.ModelSelectionInput

func selectRunModel(cfg *config.Config, in modelSelectionInput) (modelSelection, modelpolicy.Policy, error) {
	return runtime.SelectRunModel(cfg, in)
}
func modelPolicyForSession(sess *session.Session, agent *agents.Agent) modelpolicy.Policy {
	return runtime.ModelPolicyForSession(sess, agent)
}

// wireStaticAgentModelAdmission binds the same selected model and rule chain
// to every agent-launch tool of a one-shot runtime.
func wireStaticAgentModelAdmission(cfg *config.Config, mgr *tools.ToolManager, spawn *appserve.SpawnAgentRunner, policy modelpolicy.Policy, selected modelSelection) {
	state := func() (modelpolicy.Policy, runpkg.ParentModel) {
		return policy, runpkg.ParentModel{Provider: selected.Provider, Model: selected.Model}
	}
	if spawn != nil {
		spawn.SetModelPolicySource(func() appserve.ParentModelState {
			p, parent := state()
			return appserve.ParentModelState{Policy: p, Provider: parent.Provider, Model: parent.Model}
		})
	}
	if mgr != nil && mgr.Registry != nil {
		mgr.Registry.SetAgentModelAdmission(state, func(ctx context.Context, name, model string, inherited modelpolicy.Policy, parent runpkg.ParentModel) error {
			return appserve.NewRunner(cfg, appserve.RunnerOptions{}).CheckAgentModel(ctx, runpkg.Request{AgentName: name, Model: model, ModelPolicy: inherited, ParentModel: parent})
		})
	}
}

func loadPersistedAgent(name string, cfg *config.Config) (*agents.Agent, error) {
	return runtime.LoadPersistedAgent(name, cfg)
}
