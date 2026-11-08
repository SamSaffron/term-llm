//go:build windows

package atomicfile

import (
	"fmt"

	"golang.org/x/sys/windows"
)

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8
)

// Replace renames tempPath onto path with MOVEFILE_WRITE_THROUGH. Windows has
// no directory fsync: FlushFileBuffers on a directory handle returns
// ERROR_ACCESS_DENIED, and the documented durability primitive for a rename is
// the WRITE_THROUGH flag, which waits until the move is flushed to disk.
func Replace(tempPath, path string) error {
	from, err := windows.UTF16PtrFromString(tempPath)
	if err != nil {
		return fmt.Errorf("encode temp path: %w", err)
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode target path: %w", err)
	}
	if err := windows.MoveFileEx(from, to, moveFileReplaceExisting|moveFileWriteThrough); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	return nil
}

// SyncDir is a no-op on Windows; directory handles cannot be flushed and
// Replace uses MOVEFILE_WRITE_THROUGH instead.
func SyncDir(string) error { return nil }
