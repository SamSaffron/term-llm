package sqliteutil

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// FileURI converts a filesystem path into a SQLite file: URI. Windows drive and
// UNC paths are normalized so the URI stays valid on every platform, and the
// conversion is deliberately independent of the running GOOS so it stays
// testable on any builder.
func FileURI(path string) string {
	slashPath := filepath.ToSlash(path)
	windowsDrivePath := len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
	windowsUNCPath := strings.HasPrefix(path, `\\`)
	if windowsDrivePath || windowsUNCPath {
		slashPath = strings.ReplaceAll(path, `\`, "/")
	}

	u := url.URL{Scheme: "file", Path: slashPath}
	// Drive-letter paths need a leading slash. UNC paths retain their leading
	// double slash as an empty-authority file URI (file:////server/share), since
	// stock SQLite rejects non-empty authorities other than localhost.
	if windowsDrivePath && !strings.HasPrefix(slashPath, "/") {
		u.Path = "/" + slashPath
	}
	return u.String()
}

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
