package run

import (
	"context"
	"time"
)

// DefaultRunnerCleanupTimeout bounds how long EventPipe adapters wait for a
// cancelled Runner.Run to finish its own cleanup before detaching. The runner
// goroutine may still be stuck, but callers must not let that wedge UI/session
// goroutines that already cancelled their stream context.
const DefaultRunnerCleanupTimeout = 5 * time.Second

// WaitForRunnerDone waits for Runner.Run to return after its stream context has
// been cancelled. It deliberately starts a fresh timeout and ignores ctx.Done(),
// because callers commonly pass a run context they have just cancelled and still
// need to allow a small cleanup grace period.
func WaitForRunnerDone(ctx context.Context, done <-chan struct{}, timeout time.Duration) bool {
	if done == nil {
		return true
	}
	if timeout <= 0 {
		timeout = DefaultRunnerCleanupTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	select {
	case <-done:
		return true
	case <-cleanupCtx.Done():
		return false
	}
}
