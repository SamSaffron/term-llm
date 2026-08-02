//go:build !windows

package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func lockGatewayClientStore(lockPath string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create gateway state directory: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open gateway client lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire gateway client lock: %w", err)
	}
	return func() error {
		unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		closeErr := file.Close()
		if unlockErr != nil {
			return fmt.Errorf("unlock gateway client store: %w", unlockErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close gateway client lock: %w", closeErr)
		}
		return nil
	}, nil
}
