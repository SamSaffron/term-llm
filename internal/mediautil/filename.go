package mediautil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SanitizeFilename converts text to a conservative, portable filename fragment.
func SanitizeFilename(s string) string {
	replacer := strings.NewReplacer(" ", "_", "/", "", "\\", "", ":", "", "?", "", "*", "", "\"", "", "<", "", ">", "", "|", "")
	s = replacer.Replace(s)
	var b strings.Builder
	b.Grow(len(s))
	lastUnderscore := false
	for _, r := range strings.ToLower(s) {
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlphaNum || r == '-' {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if r == '_' && !lastUnderscore {
			b.WriteRune(r)
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

// SaveFile writes generated media to outputDir, falling back to defaultDir.
func SaveFile(data []byte, outputDir, defaultDir, filename, mediaKind string) (string, error) {
	dir := ExpandPath(outputDir)
	if dir == "" {
		dir = ExpandPath(defaultDir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", mediaKind, err)
	}
	return path, nil
}

// ExpandPath expands a leading ~/ using the current user's home directory.
func ExpandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
