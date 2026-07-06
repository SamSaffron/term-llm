package servehttp

import (
	"fmt"
	"strings"
)

// NormalizeBasePath validates and normalizes a base-path value.
// It ensures a leading slash, strips trailing slashes, and rejects empty or
// root-only paths (use the default "/ui" instead of "/").
func NormalizeBasePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", fmt.Errorf("--base-path must not be empty")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "", fmt.Errorf("--base-path must not be \"/\" (use the default /ui or a sub-path like /chat)")
	}
	return p, nil
}
