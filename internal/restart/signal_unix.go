//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package restart

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// Listen registers only SIGUSR2. Stop joins dispatch without changing INT, TERM,
// HUP, USR1 or terminal signal handling. Call at executable entry, before command
// parsing, configuration loading or tool launch.
func (d *Dispatcher) Listen() func() {
	ch := make(chan os.Signal, 1)
	done, joined := make(chan struct{}), make(chan struct{})
	signal.Notify(ch, syscall.SIGUSR2)
	go func() {
		defer close(joined)
		for {
			select {
			case <-done:
				return
			case <-ch:
				d.Request()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { signal.Stop(ch); close(done); <-joined; d.Finish() }) }
}
