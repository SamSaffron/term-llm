//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package cmd

import (
	"context"
	"os"
	ossignal "os/signal"
	"syscall"
)

func execWebProcess(path string, args, env []string) error { return syscall.Exec(path, args, env) }

func installWebExecSignal(ctx context.Context, c *webExecCoordinator) func() {
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan os.Signal, 1)
	ossignal.Notify(ch, syscall.SIGUSR2)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				c.request()
			}
		}
	}()
	return func() { ossignal.Stop(ch); cancel() }
}
