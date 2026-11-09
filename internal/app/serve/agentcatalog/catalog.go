package agentcatalog

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
)

// Entry is one agent in the catalog.
type Entry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Source is "user" or "builtin", matching `term-llm agents --user/--builtin`.
	Source   string `json:"source"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Response is the GET /v1/agents body.
type Response struct {
	Agents []Entry `json:"agents"`
}

// List returns the same agents as `term-llm agents`: user agents shadow
// built-ins of the same name, and project agents are never discovered.
func List(cfg *config.Config) (Response, error) {
	var registryCfg agents.RegistryConfig
	if cfg != nil {
		registryCfg = agents.RegistryConfig{UseBuiltin: cfg.Agents.UseBuiltin, SearchPaths: cfg.Agents.SearchPaths}
	}
	registry, err := agents.NewRegistry(registryCfg)
	if err != nil {
		return Response{}, fmt.Errorf("create agent registry: %w", err)
	}
	list, err := registry.List()
	if err != nil {
		return Response{}, fmt.Errorf("list agents: %w", err)
	}
	items := make([]Entry, 0, len(list))
	for _, agent := range list {
		items = append(items, Entry{
			Name:        agent.Name,
			Description: agent.Description,
			Source:      agent.Source.SourceName(),
			Provider:    agent.Provider,
			Model:       agent.Model,
		})
	}
	return Response{Agents: items}, nil
}
