//go:build windows

package credentials

import "github.com/samsaffron/term-llm/internal/filelock"

func lockChatGPTCredentials(lockPath string) (func() error, error) {
	return filelock.Lock(lockPath)
}
