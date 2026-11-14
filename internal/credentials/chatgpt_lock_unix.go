//go:build !windows

package credentials

import "github.com/term-llm/term-llm/internal/filelock"

func lockChatGPTCredentials(lockPath string) (func() error, error) {
	return filelock.Lock(lockPath)
}
