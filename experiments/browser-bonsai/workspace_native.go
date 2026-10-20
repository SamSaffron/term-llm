//go:build !js && !wasip1

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Native tests use a temporary directory as the workspace, mirroring the
// browser's memfs workspace. Writes and deletes are confined to that root and
// the root itself is never removed.

func snapshotWorkspaceFiles(root string) (map[string]string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	files := map[string]string{}
	err = filepath.WalkDir(rootAbs, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		data, readErr := os.ReadFile(name)
		if readErr != nil {
			return readErr
		}
		if !utf8.Valid(data) {
			return nil
		}
		rel, relErr := filepath.Rel(rootAbs, name)
		if relErr != nil {
			return relErr
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("snapshot workspace: %w", err)
	}
	return files, nil
}

func writeWorkspaceFile(root, rel, content string) error {
	full, err := nativeWorkspacePath(root, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

func readWorkspaceFile(root, rel string) (string, error) {
	full, err := nativeWorkspacePath(root, rel)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s is not a UTF-8 text file", rel)
	}
	return string(data), nil
}

func removeWorkspaceFile(root, rel string) error {
	full, err := nativeWorkspacePath(root, rel)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	pruneEmptyWorkspaceDirs(root, filepath.Dir(full))
	return nil
}

func nativeWorkspacePath(root, rel string) (string, error) {
	clean, err := cleanWorkspaceRelative(rel)
	if err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	full := filepath.Join(rootAbs, filepath.FromSlash(clean))
	if full == rootAbs || !strings.HasPrefix(full, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the workspace: %q", rel)
	}
	return full, nil
}

// pruneEmptyWorkspaceDirs removes empty parents left behind by deletes and
// stops at the workspace root. It never removes the root itself.
func pruneEmptyWorkspaceDirs(root, dir string) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return
	}
	for {
		dir = filepath.Clean(dir)
		if dir == rootAbs || !strings.HasPrefix(dir, rootAbs+string(filepath.Separator)) {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
