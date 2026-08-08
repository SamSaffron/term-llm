package sqliteutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveDBPathOverride expands and absolutizes an optional SQLite database path.
// An empty override delegates to defaultPath; :memory: is returned unchanged.
func ResolveDBPathOverride(pathOverride string, defaultPath func() (string, error)) (string, error) {
	pathOverride = strings.TrimSpace(pathOverride)
	if pathOverride == "" {
		return defaultPath()
	}
	if pathOverride == ":memory:" {
		return pathOverride, nil
	}
	pathOverride = os.ExpandEnv(pathOverride)
	if strings.HasPrefix(pathOverride, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		pathOverride = filepath.Join(homeDir, pathOverride[2:])
	}
	abs, err := filepath.Abs(pathOverride)
	if err != nil {
		return "", fmt.Errorf("resolve db path %q: %w", pathOverride, err)
	}
	return abs, nil
}
