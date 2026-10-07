package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type modelSelection struct{ Provider, Model string }
type modelSelectionInput struct {
	Agent                                              *agents.Agent
	CmdProvider, CmdModel, ProviderFlag, ModelOverride string
	Fast                                               bool
	Inherited                                          modelpolicy.Policy
	Parent                                             runpkg.ParentModel
	ResumeModel                                        runpkg.ParentModel
}

// selectRunModel is the single selection/admission path for CLI and delegated
// launches. The caller passes an isolated config clone; both policy and provider
// construction use the returned concrete selection.
func selectRunModel(cfg *config.Config, in modelSelectionInput) (modelSelection, modelpolicy.Policy, error) {
	agentName, agentProvider, agentModel := "", "", ""
	if in.Agent != nil {
		agentName, agentProvider, agentModel = in.Agent.Name, in.Agent.Provider, in.Agent.Model
	}
	providerFlag := in.ProviderFlag
	if in.ResumeModel.Provider != "" && providerFlag == "" && in.ModelOverride == "" {
		// A continuation keeps the session's admitted identity even when the
		// agent definition has since changed its default model.
		providerFlag = in.ResumeModel.Provider + ":" + in.ResumeModel.Model
	} else if agentProvider == "" && agentModel == "" && providerFlag == "" && in.ModelOverride == "" && in.Parent.Provider != "" {
		providerFlag = in.Parent.Provider + ":" + in.Parent.Model
	}
	if err := applyProviderOverridesWithAgent(cfg, in.CmdProvider, in.CmdModel, providerFlag, agentProvider, agentModel); err != nil {
		return modelSelection{}, modelpolicy.Policy{}, err
	}
	if in.ModelOverride != "" {
		if err := applyAgentModelOverride(cfg, in.ModelOverride); err != nil {
			return modelSelection{}, modelpolicy.Policy{}, fmt.Errorf("apply model override %q: %w", in.ModelOverride, err)
		}
	}
	selected := modelSelection{Provider: strings.TrimSpace(cfg.DefaultProvider), Model: strings.TrimSpace(activeModel(cfg))}
	if in.Fast {
		key, model, ok := llm.ResolveFastTarget(cfg, selected.Provider)
		if !ok {
			return modelSelection{}, modelpolicy.Policy{}, fmt.Errorf("no fast model configured for provider %q", selected.Provider)
		}
		selected = modelSelection{Provider: key, Model: model}
	}
	policy := in.Inherited
	if in.Agent != nil {
		policy = policy.With(agentName, in.Agent.AllowedModels)
	}
	if err := policy.CheckWithConfig(cfg, agentName, selected.Provider, selected.Model); err != nil {
		return selected, policy, err
	}
	return selected, policy, nil
}

func modelPolicyForSession(sess *session.Session, agent *agents.Agent) modelpolicy.Policy {
	var policy modelpolicy.Policy
	if sess != nil {
		policy = sess.ModelPolicy
	}
	if agent != nil {
		policy = policy.With(agent.Name, agent.AllowedModels)
	}
	return policy
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
