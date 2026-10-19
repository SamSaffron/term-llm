package runtime

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/app/agent"
	"github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

type ModelSelection struct{ Provider, Model string }
type ModelSelectionInput struct {
	Agent                                              *agents.Agent
	CmdProvider, CmdModel, ProviderFlag, ModelOverride string
	Fast                                               bool
	Inherited                                          modelpolicy.Policy
	Parent                                             runpkg.ParentModel
	ResumeModel                                        runpkg.ParentModel
}

// SelectRunModel is the single selection/admission path for CLI and delegated
// launches. The caller passes an isolated config clone; both policy and provider
// construction use the returned concrete selection.
func SelectRunModel(cfg *config.Config, in ModelSelectionInput) (ModelSelection, modelpolicy.Policy, error) {
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
	if err := bootstrap.ApplyProviderOverridesWithAgent(cfg, in.CmdProvider, in.CmdModel, providerFlag, agentProvider, agentModel); err != nil {
		return ModelSelection{}, modelpolicy.Policy{}, err
	}
	if in.ModelOverride != "" {
		if err := agent.ApplyModelOverride(cfg, in.ModelOverride); err != nil {
			return ModelSelection{}, modelpolicy.Policy{}, fmt.Errorf("apply model override %q: %w", in.ModelOverride, err)
		}
	}
	selected := ModelSelection{Provider: strings.TrimSpace(cfg.DefaultProvider), Model: strings.TrimSpace(bootstrap.ActiveModel(cfg))}
	if in.Fast {
		key, model, ok := llm.ResolveFastTarget(cfg, selected.Provider)
		if !ok {
			return ModelSelection{}, modelpolicy.Policy{}, fmt.Errorf("no fast model configured for provider %q", selected.Provider)
		}
		selected = ModelSelection{Provider: key, Model: model}
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

func ModelPolicyForSession(sess *session.Session, agent *agents.Agent) modelpolicy.Policy {
	var policy modelpolicy.Policy
	if sess != nil {
		policy = sess.ModelPolicy
	}
	if agent != nil {
		policy = policy.With(agent.Name, agent.AllowedModels)
	}
	return policy
}
