package main

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// The shell bridge exchanges a full workspace snapshot with the page. Paths
// crossing that boundary are always slash-separated and relative to the
// workspace root; anything else is rejected before it can touch the filesystem.

// workspaceRelativePath resolves a tool-supplied path (relative, or absolute
// inside root) to a clean slash-separated path relative to the workspace.
func workspaceRelativePath(root, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("path is required")
	}
	rootSlash := path.Clean(filepath.ToSlash(root))
	candidate := filepath.ToSlash(name)
	if path.IsAbs(candidate) {
		if candidate == rootSlash {
			return "", fmt.Errorf("path must name a file inside the workspace: %s", name)
		}
		prefix := strings.TrimSuffix(rootSlash, "/") + "/"
		if !strings.HasPrefix(candidate, prefix) {
			return "", fmt.Errorf("path is outside the workspace: %s", name)
		}
		candidate = strings.TrimPrefix(candidate, prefix)
	}
	return cleanWorkspaceRelative(candidate)
}

// cleanWorkspaceRelative rejects traversal, absolute paths, Windows separators
// and non-canonical forms. Strictness matters most for the sandbox's returned
// file map, which is applied to the workspace without further interpretation.
func cleanWorkspaceRelative(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("file path is empty")
	}
	if strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("file path must use forward slashes: %q", name)
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("file path contains a NUL byte")
	}
	if path.IsAbs(name) {
		return "", fmt.Errorf("file path must be relative to the workspace: %q", name)
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("file path escapes the workspace: %q", name)
	}
	if cleaned != name {
		return "", fmt.Errorf("file path is not canonical (%q becomes %q)", name, cleaned)
	}
	return cleaned, nil
}

// joinWorkspacePath joins a workspace root and a validated relative path.
func joinWorkspacePath(root, rel string) string {
	return path.Join(filepath.ToSlash(root), rel)
}
