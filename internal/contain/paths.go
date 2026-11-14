package contain

import (
	"path/filepath"

	"github.com/term-llm/term-llm/internal/config"
)

// ContainersRoot returns the global directory containing named contain workspaces.
func ContainersRoot() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "containers"), nil
}

// ContainerDir returns the configuration directory for a named contain workspace.
func ContainerDir(name string) (string, error) {
	root, err := ContainersRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}

// ComposePath returns the canonical compose.yaml path for a named contain workspace.
func ComposePath(name string) (string, error) {
	dir, err := ContainerDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "compose.yaml"), nil
}
