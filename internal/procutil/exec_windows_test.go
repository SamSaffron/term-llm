//go:build windows

package procutil

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigureDetachedCommandCreatesProcessGroupWithoutWindow(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	ConfigureDetachedCommand(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	if cmd.SysProcAttr.CreationFlags&createNewProcessGroup == 0 {
		t.Fatalf("CreationFlags = %#x, want CREATE_NEW_PROCESS_GROUP", cmd.SysProcAttr.CreationFlags)
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("CreationFlags = %#x, want CREATE_NO_WINDOW", cmd.SysProcAttr.CreationFlags)
	}
	if cmd.Cancel == nil {
		t.Fatal("expected process-tree cancellation hook")
	}
}

func TestConfigureCommandProcessGroupKeepsConsole(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	ConfigureCommandProcessGroup(cmd)

	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&createNewProcessGroup == 0 {
		t.Fatalf("SysProcAttr = %#v, want CREATE_NEW_PROCESS_GROUP", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow != 0 {
		t.Fatal("credential-capable process group must keep the caller's console")
	}
}

func TestKillCommandTreeTerminatesChild(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "ping -n 30 127.0.0.1")
	ConfigureDetachedCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	if err := KillCommandTree(cmd); err != nil {
		t.Fatalf("KillCommandTree: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process tree survived KillCommandTree")
	}
}

func TestConfigureLeavesPlainCommandCancelNil(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	ConfigureDetachedCommand(cmd)
	if cmd.Cancel != nil {
		t.Fatal("plain exec.Command must not receive a Cancel hook")
	}
}

func TestKillCommandTreeTerminatesGrandchild(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "termllm-tree-child.exe")
	src := filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("read %s: %v", src, err)
	}
	if err := os.WriteFile(helper, data, 0o700); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", `"`+helper+`" -n 30 127.0.0.1`)
	ConfigureDetachedCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for !processImageRunning(t, "termllm-tree-child.exe") && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if !processImageRunning(t, "termllm-tree-child.exe") {
		_ = KillCommandTree(cmd)
		t.Skip("helper process did not start in this environment")
	}
	if err := KillCommandTree(cmd); err != nil {
		t.Fatalf("KillCommandTree: %v", err)
	}
	for time.Now().Before(deadline) {
		if !processImageRunning(t, "termllm-tree-child.exe") {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("parent command did not exit after tree kill")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("grandchild survived KillCommandTree")
}

func processImageRunning(t *testing.T, image string) bool {
	t.Helper()
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+image, "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		t.Fatalf("tasklist: %v (%s)", err, out)
	}
	return bytes.Contains(out, []byte(image))
}
