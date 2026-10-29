package session

import (
	"fmt"
	"os"
	"path/filepath"
)

// prepareSessionStorage hardens writable storage once, before SQLite opens it.
// New WAL/SHM files inherit the DB mode; surviving sidecars need explicit repair.
func prepareSessionStorage(path string, readOnly bool) error {
	if path == ":memory:" || readOnly {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	// Repair only our default directory, not custom parents or symlink targets.
	if defaultPath, err := GetDBPath(); err == nil && dir == filepath.Dir(defaultPath) {
		info, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("stat data directory: %w", err)
		}
		if info.IsDir() {
			if err := os.Chmod(dir, 0o700); err != nil {
				return fmt.Errorf("secure data directory: %w", err)
			}
		}
	}

	// Pre-create privately even under umask 000, without truncating existing DBs.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil && !os.IsExist(err) {
		return fmt.Errorf("create private database: %w", err)
	}
	if err == nil {
		if err := file.Close(); err != nil {
			return fmt.Errorf("close private database: %w", err)
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := secureSessionFile(path + suffix); err != nil {
			return err
		}
	}
	return nil
}

func secureSessionFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil // SQLite has not created this sidecar yet.
	}
	if err != nil {
		return fmt.Errorf("stat session file %s: %w", path, err)
	}
	// Do not chmod through symlinks or special files to unrelated user data.
	if !info.Mode().IsRegular() {
		return fmt.Errorf("secure session file %s: not a regular file", path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure session file %s: %w", path, err)
	}
	return nil
}
