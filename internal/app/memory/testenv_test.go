package memory

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
)

// testEnv returns an Env wired with minimal, config-only dependencies.
func testEnv(dbPath, agent string) Env {
	return Env{
		Agent:  agent,
		DBPath: dbPath,
		Deps: Deps{
			LoadConfig: func() (*config.Config, error) {
				cfg, err := config.Load()
				if err != nil {
					return nil, fmt.Errorf("failed to load config: %w", err)
				}
				return cfg, nil
			},
			ApplyAskProviderOverrides: func(cfg *config.Config) error {
				cfg.ApplyOverrides(cfg.Ask.Provider, cfg.Ask.Model)
				return nil
			},
			NewEngine: func(provider core.Provider, cfg *config.Config) *engine.Engine {
				return engine.NewEngine(provider, engine.NewToolRegistry())
			},
			ParseTags: func(tags string) []string {
				var out []string
				for _, tag := range strings.Split(tags, ",") {
					if tag = strings.ToLower(strings.TrimSpace(tag)); tag != "" {
						out = append(out, tag)
					}
				}
				return out
			},
			LoadAgent: func(name string, cfg *config.Config) (*agents.Agent, error) {
				if !agents.IsAgentPath(name) {
					return nil, fmt.Errorf("agent %q not found", name)
				}
				agent, err := agents.LoadFromPath(name)
				if err != nil {
					return nil, fmt.Errorf("load agent from path: %w", err)
				}
				return agent, nil
			},
			SessionStoreConfig: func(cfg *config.Config) session.Config {
				return session.Config{
					Enabled: cfg.Sessions.Enabled,
					Path:    cfg.Sessions.Path,
				}
			},
		},
	}
}
