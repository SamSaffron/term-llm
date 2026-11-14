//go:build !windows

package widgets

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/procutil"
)

func TestWidgetStopProcessReapsChildProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sleep 60 & echo $! > %s; wait", pidFile))
	owned, err := procutil.PrepareCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := owned.Start(); err != nil {
		t.Fatal(err)
	}
	pid := waitWidgetPID(t, pidFile)
	done := make(chan struct{})
	go func() { _ = owned.Wait(); close(done) }()
	e := &widgetEntry{
		manifest: &Manifest{ID: "test-widget", Mount: "test-widget", Dir: dir},
		state:    stateRunning,
		owned:    owned,
		procDone: done,
	}
	if err := e.stopProcess(); err != nil {
		t.Fatalf("stopProcess: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("widget child %d survived stop", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWidgetStopKeepsSocketUntilExitAcknowledged(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(fmt.Sprint("acknowledged=", acknowledged), func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "widget.sock")
			id, err := filepath.Rel(socketRuntimeDir, strings.TrimSuffix(socket, ".sock"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(socket, nil, 0600); err != nil {
				t.Fatal(err)
			}
			child := exec.Command("sleep", "60")
			owned, err := procutil.PrepareCommand(child)
			if err != nil {
				t.Fatal(err)
			}
			if err := owned.Start(); err != nil {
				t.Fatal(err)
			}
			var done chan struct{}
			if acknowledged {
				done = make(chan struct{})
				go func() { _ = owned.Wait(); close(done) }()
				t.Cleanup(func() { _ = owned.Close(); <-done })
			} else {
				t.Cleanup(func() { _ = owned.Close(); _ = owned.Wait() })
			}
			e := &widgetEntry{manifest: &Manifest{ID: id, Command: []string{"fixture", "$SOCKET"}}, owned: owned, procDone: done, state: stateRunning}
			err = e.stopProcess()
			_, statErr := os.Stat(socket)
			if acknowledged {
				if err != nil || !os.IsNotExist(statErr) {
					t.Fatalf("acknowledged exit did not remove socket: %v %v", err, statErr)
				}
			} else {
				if err == nil || statErr != nil || e.owned != owned {
					t.Fatalf("unacknowledged exit lost process/socket: %v %v", err, statErr)
				}
			}
		})
	}
}

func waitWidgetPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid file %s was not written", path)
	return 0
}
