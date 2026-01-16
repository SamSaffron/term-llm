package agents

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

//go:embed builtin/*/agent.yaml builtin/*/system.md
var builtinFS embed.FS

// builtinAgentNames lists all built-in agent names.
var builtinAgentNames = []string{
	"codebase",
	"commit",
	"editor",
	"researcher",
	"reviewer",
	"shell",
}

// getBuiltinAgent loads a built-in agent by name.
func getBuiltinAgent(name string) (*Agent, error) {
	agentYAML, err := builtinFS.ReadFile(fmt.Sprintf("builtin/%s/agent.yaml", name))
	if err != nil {
		return nil, fmt.Errorf("builtin agent %s not found", name)
	}

	systemMD, _ := builtinFS.ReadFile(fmt.Sprintf("builtin/%s/system.md", name))

	return LoadFromEmbedded(name, agentYAML, systemMD)
}

// getBuiltinAgents returns all built-in agents.
func getBuiltinAgents() []*Agent {
	var agents []*Agent
	for _, name := range builtinAgentNames {
		if agent, err := getBuiltinAgent(name); err == nil {
			agents = append(agents, agent)
		}
	}
	return agents
}

// GetBuiltinAgentNames returns the names of all built-in agents.
func GetBuiltinAgentNames() []string {
	return builtinAgentNames
}

// IsBuiltinAgent checks if an agent name is a built-in.
func IsBuiltinAgent(name string) bool {
	for _, n := range builtinAgentNames {
		if n == name {
			return true
		}
	}
	return false
}

// copyBuiltinAgent copies a built-in agent to a destination directory.
func copyBuiltinAgent(name, destDir, newName string) error {
	// Read embedded files
	agentYAML, err := builtinFS.ReadFile(fmt.Sprintf("builtin/%s/agent.yaml", name))
	if err != nil {
		return fmt.Errorf("read agent.yaml: %w", err)
	}

	systemMD, _ := builtinFS.ReadFile(fmt.Sprintf("builtin/%s/system.md", name))

	// If renaming, parse and re-serialize with new name
	if newName != name {
		var agent Agent
		if err := yaml.Unmarshal(agentYAML, &agent); err != nil {
			return fmt.Errorf("parse agent.yaml: %w", err)
		}
		agent.Name = newName
		agentYAML, err = yaml.Marshal(&agent)
		if err != nil {
			return fmt.Errorf("marshal agent.yaml: %w", err)
		}
	}

	// Write files
	if err := os.WriteFile(filepath.Join(destDir, "agent.yaml"), agentYAML, 0644); err != nil {
		return fmt.Errorf("write agent.yaml: %w", err)
	}

	if len(systemMD) > 0 {
		if err := os.WriteFile(filepath.Join(destDir, "system.md"), systemMD, 0644); err != nil {
			return fmt.Errorf("write system.md: %w", err)
		}
	}

	return nil
}
