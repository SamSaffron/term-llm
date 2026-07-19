package testutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ProcessHasExited reports whether pid no longer has executable code. On Linux,
// zombies count as exited because kill(pid, 0) continues to report them until
// their parent or init process reaps them.
func ProcessHasExited(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	if runtime.GOOS != "linux" {
		return false
	}
	state, ok := linuxProcessState(pid)
	return ok && state == 'Z'
}

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

func linuxProcessState(pid int) (byte, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	return procStatState(data)
}

func procStatState(data []byte) (byte, bool) {
	stat := string(data)
	end := strings.LastIndexByte(stat, ')')
	if end == -1 {
		return 0, false
	}
	rest := strings.TrimSpace(stat[end+1:])
	if rest == "" {
		return 0, false
	}
	return rest[0], true
}
