package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/app/runtime"
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
func wireStaticAgentModelAdmission(cfg *config.Config, mgr *tools.ToolManager, spawn *SpawnAgentRunner, policy modelpolicy.Policy, selected modelSelection) {
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
			checker := &cmdRunner{baseCfg: cfg}
			_, _, _, _, err := checker.resolveRunModel(ctx, runpkg.Request{AgentName: name, Model: model, ModelPolicy: inherited, ParentModel: parent})
			return err
		})
	}
}

// loadPersistedAgent permits a removed definition without lifting any policy
// already stored on its session. Invalid existing definitions fail closed.
func loadPersistedAgent(name string, cfg *config.Config) (*agents.Agent, error) {
	agent, err := LoadAgent(name, cfg)
	if err == nil {
		return agent, nil
	}
	if errors.Is(err, agents.ErrNotFound) {
		return nil, nil
	}
	if agents.IsAgentPath(name) {
		if _, statErr := os.Stat(filepath.Join(name, "agent.yaml")); errors.Is(statErr, os.ErrNotExist) {
			return nil, nil
		}
	}
	return nil, err
}
