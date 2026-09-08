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
func (d *Coordinator) Listen() func() {
	ch := make(chan os.Signal, 1)
	done, joined := make(chan struct{}), make(chan struct{})
	signal.Notify(ch, syscall.SIGUSR2)
	var signalMu sync.Mutex
	stopped := false
	d.mu.Lock()
	d.beforeExec = func() func() {
		signalMu.Lock()
		if !stopped {
			signal.Ignore(syscall.SIGUSR2)
		}
		signalMu.Unlock()
		return func() {
			signalMu.Lock()
			defer signalMu.Unlock()
			if !stopped {
				signal.Notify(ch, syscall.SIGUSR2)
			}
		}
	}
	d.mu.Unlock()
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
	return func() {
		once.Do(func() {
			signalMu.Lock()
			stopped = true
			signal.Stop(ch)
			close(done)
			signalMu.Unlock()
			<-joined
		})
	}
}
