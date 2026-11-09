package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/samsaffron/term-llm/internal/agents"
)

func agentSourceDirectory(agent *agents.Agent) string {
	if agent.Source == agents.SourceSearchPath {
		return filepath.Dir(agent.SourcePath)
	}
	return ""
}

func printAgentSourceHeader(agent *agents.Agent) {
	switch agent.Source {
	case agents.SourceUser:
		directory, _ := agents.GetUserAgentsDir()
		fmt.Printf("  [user] %s/\n", directory)
	case agents.SourceSearchPath:
		fmt.Printf("  [search-path] %s/\n", agentSourceDirectory(agent))
	default:
		fmt.Printf("  [%s]\n", agent.Source.SourceName())
	}
}
