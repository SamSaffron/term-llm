package cmd

import (
	"io"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

func sessionStoreOverrides() runtime.SessionStoreOverrides {
	return runtime.SessionStoreOverrides{NoSession: noSession, DBPath: sessionDBPath}
}
func sessionStoreConfig(cfg *config.Config) session.Config {
	return runtime.SessionStoreConfigWith(cfg, sessionStoreOverrides())
}
func InitSessionStore(cfg *config.Config, warnings io.Writer) (session.Store, func()) {
	return runtime.InitSessionStoreWith(cfg, warnings, sessionStoreOverrides())
}
func LoadAgent(name string, cfg *config.Config) (*agents.Agent, error) {
	return runtime.LoadAgent(name, cfg)
}
func ListAgentNames(cfg *config.Config) ([]string, error) { return runtime.ListAgentNames(cfg) }
func promptForReset() bool                                { return runtime.PromptForReset() }
