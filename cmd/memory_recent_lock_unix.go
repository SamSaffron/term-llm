//go:build !windows

package cmd

import "github.com/samsaffron/term-llm/internal/filelock"

func lockRecentFile(lockPath string) (func() error, error) {
	return filelock.Lock(lockPath)
}
