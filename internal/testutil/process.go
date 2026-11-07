package testutil

import (
	"testing"
	"time"
)

// WaitForProcessExit waits until pid is gone or is a zombie.
func WaitForProcessExit(t testing.TB, pid int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ProcessHasExited(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for process %d to exit", pid)
}
