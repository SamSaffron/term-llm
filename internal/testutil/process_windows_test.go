//go:build windows

package testutil

import (
	"os/exec"
	"testing"
	"time"
)

func TestProcessHasExited(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "ping -n 30 127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	if ProcessHasExited(pid) {
		_ = cmd.Process.Kill()
		t.Fatal("running process reported as exited")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ProcessHasExited(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d still reported alive after kill", pid)
}
