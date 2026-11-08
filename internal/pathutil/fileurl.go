package pathutil

import (
	"fmt"
	"net/url"
	"strings"
)

// ParseLocalFileURL parses a local file:// URL while leaving config-style
// environment expansion, reading, and fragment lookup to the caller. Standard
// RFC forms are parsed and percent-decoded by net/url, then normalized for the
// host platform. Practical Windows raw paths with backslashes are returned
// without unescaping.
func ParseLocalFileURL(fileURL string) (path, fragment string, err error) {
	raw := strings.TrimPrefix(fileURL, "file://")
	if i := strings.Index(raw, "#"); i >= 0 {
		raw, fragment = raw[:i], raw[i+1:]
	}
	if native, ok := parsePracticalFileURL(raw); ok {
		return native, fragment, nil
	}

	u, err := url.Parse(fileURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid file:// URL: %w", err)
	}
	if u.Host != "" && u.Host != "localhost" {
		if native, ok := parseNativeFileURLHost(u.Host, u.Path); ok {
			return native, u.Fragment, nil
		}
		return "", "", fmt.Errorf("file:// URL host must be empty or localhost: %s", fileURL)
	}
	return normalizeNativeFileURLPath(u.Path), u.Fragment, nil
}
