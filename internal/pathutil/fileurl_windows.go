//go:build windows

package pathutil

import "strings"

// parsePracticalFileURL accepts only the raw Windows forms users write. RFC
// forms such as file:///C:/A%20B must reach net/url so escapes are decoded and
// malformed escapes are rejected.
func parsePracticalFileURL(raw string) (string, bool) {
	if strings.Contains(raw, `\`) {
		if isWindowsDrivePath(raw) || strings.HasPrefix(raw, `\\`) {
			return raw, true
		}
		if len(raw) >= 3 && raw[0] == '/' && isWindowsDrivePath(raw[1:]) {
			return raw[1:], true
		}
		return "", false
	}
	if isWindowsDrivePath(raw) {
		return raw, true
	}
	return "", false
}

func parseNativeFileURLHost(host, path string) (string, bool) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", false
	}
	path = strings.ReplaceAll(path, "/", `\`)
	if !strings.HasPrefix(path, `\`) {
		path = `\` + path
	}
	return `\\` + host + path, true
}

func normalizeNativeFileURLPath(path string) string {
	// Empty-authority UNC URLs are parsed and unescaped before converting the
	// decoded //server/share path to Windows separators.
	if strings.HasPrefix(path, "//") {
		return `\\` + strings.ReplaceAll(strings.TrimPrefix(path, "//"), "/", `\`)
	}
	if len(path) >= 3 && path[0] == '/' && isWindowsDrivePath(path[1:]) {
		return path[1:]
	}
	return path
}

func isWindowsDrivePath(p string) bool {
	return len(p) >= 2 && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) && p[1] == ':'
}
