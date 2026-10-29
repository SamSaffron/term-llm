package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WritePrivateConfigFile atomically replaces a config file with mode 0600,
// regardless of its previous mode. Existing final-path symlinks are followed so
// dotfile-managed configs keep their links. Unlike WriteFileAtomically, this is
// only for configuration that may contain literal credentials.
func WritePrivateConfigFile(path string, data []byte) error {
	target, err := resolveFinalSymlink(path)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	return writePrivateConfigTarget(target, data)
}

// writePrivateConfigTarget does not follow a final symlink. It is also used on
// the pinned editor target, where following a new link could affect an unrelated
// file. Replacing rather than chmodding avoids changing hardlinked files.
func writePrivateConfigTarget(path string, data []byte) error {
	if err := checkRegularConfigTarget(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	return writeFileAtomically(path, data, 0o600)
}

func checkRegularConfigTarget(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("config target %s is not a regular file", path)
	}
	return nil
}

// EditPrivateConfigFile hardens an app-owned config before and after invoking
// edit with its logical path. Missing files start with initialContent. Only
// regular files and existing final-path symlinks to regular files are supported.
// An editor may replace the regular target, but retargeting the logical path is
// not followed on exit. Deletion is respected, and editor errors are preserved
// even if exit hardening also fails. Editor backups are outside app ownership.
func EditPrivateConfigFile(path string, initialContent []byte, edit func(string) error) error {
	target, err := resolveFinalSymlink(path)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	data := initialContent
	if err := checkRegularConfigTarget(target); err == nil {
		data, err = os.ReadFile(target)
		if err != nil {
			return fmt.Errorf("read config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := WritePrivateConfigFile(path, data); err != nil {
		return fmt.Errorf("harden config before editing: %w", err)
	}
	// Pin the full resolved path, including parent directory symlinks. Do not
	// follow a different target chosen by an editor when repairing permissions.
	target, err = filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve editor config path: %w", err)
	}
	editorErr := edit(path)
	if err := hardenEditedConfig(path, target); err != nil {
		return errors.Join(editorErr, fmt.Errorf("harden config after editing: %w", err))
	}
	return editorErr
}

func hardenEditedConfig(path, target string) error {
	current, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // Do not recreate a file intentionally removed by the editor.
	}
	if err != nil {
		return fmt.Errorf("resolve edited config: %w", err)
	}
	if current != target {
		return fmt.Errorf("config path changed during editing; refusing to harden a different target")
	}
	if err := checkRegularConfigTarget(target); err != nil {
		return err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("read edited config: %w", err)
	}
	return writePrivateConfigTarget(target, data)
}
