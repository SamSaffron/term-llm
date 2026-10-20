package cmd

import (
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	"github.com/samsaffron/term-llm/internal/session"
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

func loadPersistedAgent(name string, cfg *config.Config) (*agents.Agent, error) {
	return runtime.LoadPersistedAgent(name, cfg)
}
