package cmd

import (
	"fmt"
	"os"
	"path/filepath"
)

func withLockedRecentFile(recentPath string, fn func() error) (err error) {
	if err := os.MkdirAll(filepath.Dir(recentPath), 0o755); err != nil {
		return fmt.Errorf("create memory directory: %w", err)
	}

	unlock, err := lockRecentFile(recentPath + ".lock")
	if err != nil {
		return fmt.Errorf("lock recent.md: %w", err)
	}
	defer func() {
		if unlockErr := unlock(); err == nil && unlockErr != nil {
			err = unlockErr
		}
	}()

	return fn()
}

func writeRecentFileAtomically(recentPath, content string) error {
	dir := filepath.Dir(recentPath)
	base := filepath.Base(recentPath)
	tf, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tf.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := tf.WriteString(content); err != nil {
		_ = tf.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tf.Sync(); err != nil {
		_ = tf.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tf.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tempPath, 0o644); err != nil {
		return fmt.Errorf("set temp file permissions: %w", err)
	}
	if err := os.Rename(tempPath, recentPath); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	cleanup = false
	return nil
}
