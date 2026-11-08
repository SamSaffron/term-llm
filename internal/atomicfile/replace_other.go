//go:build !unix && !windows

package atomicfile

import "os"

// Replace renames tempPath onto path. Platforms in this group have no
// directory fsync primitive exposed by Go.
func Replace(tempPath, path string) error {
	return os.Rename(tempPath, path)
}

// SyncDir is a no-op on platforms without directory fsync support.
func SyncDir(string) error { return nil }
