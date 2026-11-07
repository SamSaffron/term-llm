//go:build !windows

package widgets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWidgetStartupTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := fmt.Sprintf("sleep 60 & echo $! > %s; sleep 60", pidFile)
	entry := &widgetEntry{manifest: &Manifest{
		ID:      "timeout-widget",
		Mount:   "timeout-widget",
		Dir:     dir,
		Command: []string{"sh", "-c", script, "$PORT"},
	}}

	if err := entry.startProcess(context.Background(), ""); err == nil {
		t.Fatal("startProcess succeeded without a listening widget")
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse child pid %q: %v", data, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			if entry.cmd != nil && entry.cmd.Process != nil {
				if err := syscall.Kill(entry.cmd.Process.Pid, 0); err == nil || err != syscall.ESRCH {
					t.Fatalf("shell leader %d survived startup timeout: %v", entry.cmd.Process.Pid, err)
				}
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("widget child %d survived startup timeout", pid)
}
