//go:build windows

package filelock

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Lock acquires an exclusive lock on path and returns a function that releases it.
func Lock(path string) (func() error, error) {
	return lock(path, false)
}

// TryLock acquires an exclusive lock without waiting. It returns an error when
// another process already holds the lock.
func TryLock(path string) (func() error, error) {
	return lock(path, true)
}

func lock(path string, nonblocking bool) (func() error, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if nonblocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &overlapped); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire file lock: %w", err)
	}
	return func() error {
		unlockErr := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
		closeErr := file.Close()
		if unlockErr != nil {
			if closeErr != nil {
				return fmt.Errorf("unlock file lock: %v; close lock file: %w", unlockErr, closeErr)
			}
			return fmt.Errorf("unlock file lock: %w", unlockErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close lock file: %w", closeErr)
		}
		return nil
	}, nil
}
