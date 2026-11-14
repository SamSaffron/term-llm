//go:build windows

package widgets

import (
	"os/exec"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/procutil"
)

func TestWidgetStopProcessKillsOwnedProcess(t *testing.T) {
	child := exec.Command("cmd", "/c", "ping -n 30 127.0.0.1")
	owned, err := procutil.PrepareCommand(child)
	if err != nil {
		t.Fatal(err)
	}
	if err := owned.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = owned.Wait(); close(done) }()
	e := &widgetEntry{
		manifest: &Manifest{ID: "test-widget", Mount: "test-widget", Dir: t.TempDir()},
		state:    stateRunning,
		owned:    owned,
		procDone: done,
	}
	if err := e.stopProcess(); err != nil {
		t.Fatalf("stopProcess: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = owned.Close()
		t.Fatal("widget process survived stop")
	}
}
