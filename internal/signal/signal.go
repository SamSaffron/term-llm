package signal

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// NotifyContext returns a context that is cancelled when SIGINT or SIGTERM is received.
// The returned stop function should be called to release resources.
func NotifyContext() (context.Context, context.CancelFunc) {
	return NotifyContextWithParent(context.Background())
}

// NotifyContextWithParent returns a context derived from parent that is also
// cancelled when SIGINT or SIGTERM is received. The returned stop function
// should be called to release resources.
func NotifyContextWithParent(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
