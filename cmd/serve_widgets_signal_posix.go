//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package cmd

import (
	"context"
	"log"
	"os"
	ossignal "os/signal"
	"syscall"

	"github.com/samsaffron/term-llm/internal/widgets"
)

// installWidgetStopSignal makes SIGUSR1 stop all widget subprocesses without
// stopping the serve process. The manager stays open, so later widget requests
// continue to start processes lazily.
func installWidgetStopSignal(ctx context.Context, manager *widgets.Manager) func() {
	ch := make(chan os.Signal, 1)
	ossignal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				log.Printf("[widget] received SIGUSR1, stopping all widgets")
				manager.StopAll()
			}
		}
	}()
	return func() { ossignal.Stop(ch) }
}
