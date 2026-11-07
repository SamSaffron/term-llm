//go:build unix

package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Replace renames tempPath onto path and fsyncs the containing directory so a
// crash cannot leave a durable rename pointing at unflushed metadata.
func Replace(tempPath, path string) error {
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if err := SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

// SyncDir flushes directory metadata to stable storage.
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}
