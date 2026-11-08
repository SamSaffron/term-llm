//go:build !windows

package widgets

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
		if !processAlive(pid) {
			if entry.process() != nil && processAlive(entry.process().Pid) {
				t.Fatalf("shell leader %d survived startup timeout", entry.process().Pid)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("widget child %d survived startup timeout", pid)
}

func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err == nil {
		state := strings.TrimSpace(string(out))
		if state == "" || strings.HasPrefix(state, "Z") {
			return false
		}
	}
	return true
}
