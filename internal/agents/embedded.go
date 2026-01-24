package agents

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed builtin/*/*.yaml builtin/*/*.md
var builtinFS embed.FS

// builtinAgentNames lists all built-in agent names.
var builtinAgentNames = []string{
	"active-review",
	"agent-builder",
	"artist",
	"changelog",
	"codebase",
	"commit-message",
	"developer",
	"editor",
	"file-organizer",
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

// GetBuiltinResourceDir returns the cache directory where builtin agent resources are extracted.
// Uses $XDG_CACHE_HOME if set, otherwise ~/.cache
func GetBuiltinResourceDir() (string, error) {
	var cacheDir string
	if xdgCache := os.Getenv("XDG_CACHE_HOME"); xdgCache != "" {
		cacheDir = xdgCache
	} else {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		cacheDir = filepath.Join(homeDir, ".cache")
	}
	return filepath.Join(cacheDir, "term-llm", "agents"), nil
}

// ExtractBuiltinResources extracts additional resource files for a builtin agent to the cache directory.
// This extracts all .md files except system.md (which is loaded into the agent struct).
func ExtractBuiltinResources(name string) (string, error) {
	if !IsBuiltinAgent(name) {
		return "", fmt.Errorf("not a builtin agent: %s", name)
	}

	resourceDir, err := GetBuiltinResourceDir()
	if err != nil {
		return "", err
	}

	agentDir := filepath.Join(resourceDir, name)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return "", fmt.Errorf("create resource dir: %w", err)
	}

	// Read all files in the agent's builtin directory
	entries, err := fs.ReadDir(builtinFS, fmt.Sprintf("builtin/%s", name))
	if err != nil {
		return "", fmt.Errorf("read builtin dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		// Skip agent.yaml and system.md (these are handled separately)
		if filename == "agent.yaml" || filename == "system.md" {
			continue
		}
		// Only extract .md files
		if !strings.HasSuffix(filename, ".md") {
			continue
		}

		content, err := builtinFS.ReadFile(fmt.Sprintf("builtin/%s/%s", name, filename))
		if err != nil {
			continue
		}

		destPath := filepath.Join(agentDir, filename)
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return "", fmt.Errorf("write %s: %w", filename, err)
		}
	}

	return agentDir, nil
}
