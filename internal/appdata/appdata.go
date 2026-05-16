package appdata

import (
	"fmt"
	"os"
	"path/filepath"
)

// GetDataDir returns the XDG data directory for term-llm.
// Uses $XDG_DATA_HOME if set, otherwise ~/.local/share.
func GetDataDir() (string, error) {
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "term-llm"), nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".local", "share", "term-llm"), nil
}
