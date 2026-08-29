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

//go:embed all:builtin
var builtinFS embed.FS

// builtinAgentNames lists all built-in agent names.
var builtinAgentNames = []string{
	"active-review",
	"agent-builder",
	"artist",
	"changelog",
	"codebase",
	"commit-message",
	"contain",
	"developer",
	"editor",
	"file-organizer",
	"planner",
	"web-researcher",
	"widget-builder",
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
// It walks the agent's builtin directory tree (preserving subdirectory layout) and copies
// .md, .yaml, and .env files (including dot-prefixed files like .template.yaml).
// agent.yaml and system.md at the root are skipped because they are loaded into
// the Agent struct.
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

	root := fmt.Sprintf("builtin/%s", name)
	walkErr := fs.WalkDir(builtinFS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(agentDir, filepath.FromSlash(rel)), 0755)
		}
		if rel == "agent.yaml" || rel == "system.md" {
			return nil
		}
		base := filepath.Base(rel)
		if !strings.HasSuffix(base, ".md") && !strings.HasSuffix(base, ".yaml") && base != ".env" {
			return nil
		}
		content, readErr := builtinFS.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read builtin resource %s: %w", path, readErr)
		}
		destPath := filepath.Join(agentDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(destPath), err)
		}
		mode := os.FileMode(0o644)
		if base == ".env" {
			mode = 0o600
		}
		if err := os.WriteFile(destPath, content, mode); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
		if err := os.Chmod(destPath, mode); err != nil {
			return fmt.Errorf("chmod %s: %w", rel, err)
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}

	return agentDir, nil
}
