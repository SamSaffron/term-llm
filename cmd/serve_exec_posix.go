//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package cmd

import (
	"context"
	"syscall"

	"github.com/samsaffron/term-llm/internal/restart"
)

func execWebProcess(path string, args, env []string) error { return syscall.Exec(path, args, env) }

func installWebExecSignal(ctx context.Context, c *webExecCoordinator) func() {
	return restart.Default.Bind(func() {
		if ctx.Err() == nil {
			c.request()
		}
	})
}
