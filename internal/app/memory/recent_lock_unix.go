//go:build !windows

package memory

import "github.com/term-llm/term-llm/internal/filelock"

func lockRecentFile(lockPath string) (func() error, error) {
	return filelock.Lock(lockPath)
}
