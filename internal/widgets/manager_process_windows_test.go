//go:build windows

package widgets

import (
	"os/exec"
	"testing"
	"time"
)

func TestWidgetStopProcessKillsProcess(t *testing.T) {
	child := exec.Command("cmd", "/c", "ping -n 30 127.0.0.1")
	if err := child.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = child.Wait(); close(done) }()
	e := &widgetEntry{
		manifest: &Manifest{ID: "test-widget", Mount: "test-widget", Dir: t.TempDir()},
		state:    stateRunning,
		proc:     child.Process,
		procDone: done,
	}
	e.stopProcess()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = child.Process.Kill()
		t.Fatal("widget process survived stop")
	}
}
