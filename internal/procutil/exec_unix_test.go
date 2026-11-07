//go:build !js && !windows

package procutil

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPrepareCommandDetachesControllingTerminal(t *testing.T) {
	cmd := exec.Command("sh", "-c", "true")
	owned, err := PrepareCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("SysProcAttr = %#v, want Setsid enabled", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.Setpgid {
		t.Fatal("Setpgid must not be combined with Setsid")
	}
}

func TestPrepareCommandProcessGroupPreservesControllingTerminal(t *testing.T) {
	cmd := exec.Command("sh", "-c", "true")
	owned, err := PrepareCommandProcessGroup(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %#v, want Setpgid enabled", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.Setsid {
		t.Fatal("credential-capable process group must not detach its terminal")
	}
}

func TestCommandRunAndOutput(t *testing.T) {
	cmd := exec.Command("sh", "-c", "printf hello")
	owned, err := PrepareCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	out, err := owned.Output()
	if err != nil || string(out) != "hello" {
		t.Fatalf("Output = %q, %v", out, err)
	}
}

func TestCommandCloseKillsProcessGroup(t *testing.T) {
	pidFile := t.TempDir() + "/child.pid"
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $! > "+pidFile+"; sleep 60")
	owned, err := PrepareCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := owned.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- owned.Wait() }()
	pid := waitForPIDFile(t, pidFile)
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait hung after Close")
	}
	if processAlive(pid) {
		t.Fatalf("child %d survived Close", pid)
	}
}

func TestCommandContextCancellationReapsGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 60 & wait")
	owned, err := PrepareCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if err := owned.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- owned.Wait() }()
	cancel()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait hung after context cancellation")
	}
}

func waitForPIDFile(t *testing.T, path string) int {
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
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s not written", path)
	return 0
}

func processAlive(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(out))
	return state != "" && !strings.HasPrefix(state, "Z")
}
