//go:build !windows

package filelock

import (
	"fmt"
	"os"
	"syscall"
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
	operation := syscall.LOCK_EX
	if nonblocking {
		operation |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire file lock: %w", err)
	}
	return func() error {
		unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
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
