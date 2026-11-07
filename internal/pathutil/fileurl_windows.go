//go:build windows

package pathutil

import "strings"

func parseNativeFileURLPath(raw string) (string, bool) {
	if isWindowsDrivePath(raw) {
		return raw, true
	}
	if strings.HasPrefix(raw, `\\`) {
		return raw, true
	}
	if strings.HasPrefix(raw, "//") {
		return `\\` + strings.TrimPrefix(raw, "//"), true
	}
	if len(raw) >= 3 && raw[0] == '/' && isWindowsDrivePath(raw[1:]) {
		return raw[1:], true
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
	if len(path) >= 3 && path[0] == '/' && isWindowsDrivePath(path[1:]) {
		return path[1:]
	}
	return path
}

func isWindowsDrivePath(p string) bool {
	return len(p) >= 2 && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) && p[1] == ':'
}
