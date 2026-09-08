//go:build !windows

package cmd

import (
	"github.com/samsaffron/term-llm/internal/restart"
	"syscall"
)

func replaceProcess(executable string, args, env []string) error {
	return restart.Default.Exec(func() error { return syscall.Exec(executable, args, env) })
}
