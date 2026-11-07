//go:build !windows

package widgets

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWidgetStopProcessReapsChildProcessGroup(t *testing.T) {
	t.Run("entire process group", func(t *testing.T) {
		testWidgetStopProcessReapsChildProcessGroup(t)
	})
}

func testWidgetStopProcessReapsChildProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-specific")
	}

	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep executable unavailable: %v", err)
	}
	leader := exec.Command(sleep, "60")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatalf("start process-group leader: %v", err)
	}
	leaderDone := make(chan struct{})
	go func() {
		_ = leader.Wait()
		close(leaderDone)
	}()

	member := exec.Command(sleep, "60")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if err := member.Start(); err != nil {
		_ = leader.Process.Kill()
		<-leaderDone
		t.Fatalf("start process-group member: %v", err)
	}
	memberDone := make(chan struct{})
	go func() {
		_ = member.Wait()
		close(memberDone)
	}()

	e := &widgetEntry{
		manifest: &Manifest{ID: "test-widget", Mount: "test-widget", Dir: t.TempDir()},
		state:    stateRunning,
		proc:     leader.Process,
		procDone: leaderDone,
	}
	e.stopProcess()

	select {
	case <-memberDone:
	case <-time.After(100 * time.Millisecond):
		_ = member.Process.Kill()
		t.Fatal("widget process-group member survived stop")
	}
}

func TestWidgetStopKeepsSocketUntilExitAcknowledged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-specific")
	}
	for _, acknowledged := range []bool{false, true} {
		t.Run(fmt.Sprint("acknowledged=", acknowledged), func(t *testing.T) {
			// Point the test manifest at a private fixture, not a real widget socket.
			socket := filepath.Join(t.TempDir(), "widget.sock")
			id, err := filepath.Rel(socketRuntimeDir, strings.TrimSuffix(socket, ".sock"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(socket, nil, 0600); err != nil {
				t.Fatal(err)
			}
			child := exec.Command("sleep", "60")
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			var done chan struct{}
			if acknowledged {
				done = make(chan struct{})
				go func() { _ = child.Wait(); close(done) }()
				t.Cleanup(func() { _ = child.Process.Kill(); <-done })
			} else {
				t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
			}
			e := &widgetEntry{manifest: &Manifest{ID: id, Command: []string{"fixture", "$SOCKET"}}, proc: child.Process, procDone: done, state: stateRunning}
			err = e.stopProcess()
			_, statErr := os.Stat(socket)
			if acknowledged {
				if err != nil || !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("acknowledged exit did not remove socket: %v %v", err, statErr)
				}
			} else {
				if err == nil || statErr != nil || e.proc != child.Process {
					t.Fatalf("unacknowledged exit lost process/socket: %v %v", err, statErr)
				}
			}
		})
	}
}
