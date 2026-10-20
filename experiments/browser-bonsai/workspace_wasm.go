//go:build js && wasm

package main

import (
	"fmt"
	"path"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/memfs"
)

// The browser workspace is the same internal/memfs tree the real file tools
// use, so sandbox file changes are immediately visible to read_file, glob, and
// the rest of the tool surface.

func snapshotWorkspaceFiles(root string) (map[string]string, error) {
	entries, err := memfs.Snapshot(root)
	if err != nil {
		return nil, fmt.Errorf("snapshot workspace: %w", err)
	}
	files := make(map[string]string, len(entries))
	for _, entry := range entries {
		data, readErr := memfs.ReadFile(joinWorkspacePath(root, entry.Path))
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Path, readErr)
		}
		if !utf8.Valid(data) {
			continue
		}
		files[entry.Path] = string(data)
	}
	return files, nil
}

func writeWorkspaceFile(root, rel, content string) error {
	full := joinWorkspacePath(root, rel)
	if dir := path.Dir(full); dir != "" {
		if err := memfs.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return memfs.WriteFile(full, []byte(content), 0o644)
}

func readWorkspaceFile(root, rel string) (string, error) {
	data, err := memfs.ReadFile(joinWorkspacePath(root, rel))
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s is not a UTF-8 text file", rel)
	}
	return string(data), nil
}

func removeWorkspaceFile(root, rel string) error {
	return memfs.Remove(joinWorkspacePath(root, rel))
}
