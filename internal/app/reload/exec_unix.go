//go:build !windows

package reload

import (
	"github.com/samsaffron/term-llm/internal/restart"
	"syscall"
)

func ReplaceProcess(executable string, args, env []string) error {
	return restart.Default.Exec(func() error { return syscall.Exec(executable, args, env) })
}
