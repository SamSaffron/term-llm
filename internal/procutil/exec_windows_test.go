//go:build windows

package procutil

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestConfigureDetachedCommandCreatesProcessGroupWithoutWindow(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	ConfigureDetachedCommand(cmd)
	defer KillCommandTree(cmd)

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
	if lookupCommandTree(cmd) == nil {
		t.Fatal("expected owned command tree")
	}
}

func TestConfigureCommandProcessGroupKeepsConsole(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	ConfigureCommandProcessGroup(cmd)
	defer KillCommandTree(cmd)

	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&createNewProcessGroup == 0 {
		t.Fatalf("SysProcAttr = %#v, want CREATE_NEW_PROCESS_GROUP", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow != 0 {
		t.Fatal("credential-capable process group must keep the caller's console")
	}
}

func TestConfigureLeavesPlainCommandCancelNil(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	ConfigureDetachedCommand(cmd)
	defer KillCommandTree(cmd)
	if cmd.Cancel != nil {
		t.Fatal("plain exec.Command must not receive a Cancel hook")
	}
}

func TestStartCommandRequiresConfiguration(t *testing.T) {
	if err := StartCommand(exec.Command("cmd", "/c", "exit 0")); err == nil {
		t.Fatal("StartCommand accepted an unconfigured command")
	}
}

func TestRunCommandResumesSuspendedProcess(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 7")
	ConfigureDetachedCommand(cmd)
	err := RunCommand(cmd)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("RunCommand error = %v, want exit status 7", err)
	}
	if err := KillCommandTree(cmd); err != nil {
		t.Fatalf("KillCommandTree after Wait: %v", err)
	}
}

func TestKillCommandTreeTerminatesChild(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "ping -n 30 127.0.0.1")
	ConfigureDetachedCommand(cmd)
	if err := StartCommand(cmd); err != nil {
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

func TestKillCommandTreeTerminatesGrandchild(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "termllm-tree-child.exe")
	copyOwnedPing(t, helper)
	pidFile := filepath.Join(dir, "child.pid")
	script := `$p=Start-Process -FilePath '` + helper + `' -ArgumentList '-n','60','127.0.0.1' -WindowStyle Hidden -PassThru; [IO.File]::WriteAllText('` + pidFile + `',[string]$p.Id); Start-Sleep -Seconds 60`

	cmd := exec.CommandContext(context.Background(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	ConfigureDetachedCommand(cmd)
	if err := StartCommand(cmd); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	pid := waitPID(t, pidFile)
	defer exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
	if err := KillCommandTree(cmd); err != nil {
		t.Fatalf("KillCommandTree: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parent command did not exit after tree kill")
	}
	if processRunningPID(t, pid) {
		t.Fatalf("grandchild %d survived KillCommandTree", pid)
	}
}

func TestKillCommandTreeAfterParentExitReapsBackgroundChild(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "termllm-parent-exit-child.exe")
	copyOwnedPing(t, helper)
	pidFile := filepath.Join(dir, "child.pid")
	script := `$p=Start-Process -FilePath '` + helper + `' -ArgumentList '-n','60','127.0.0.1' -WindowStyle Hidden -PassThru; [IO.File]::WriteAllText('` + pidFile + `',[string]$p.Id)`

	cmd := exec.CommandContext(context.Background(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	ConfigureDetachedCommand(cmd)
	if err := RunCommand(cmd); err != nil {
		t.Fatalf("parent run: %v", err)
	}
	pid := readPID(t, pidFile)
	defer exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
	if !processRunningPID(t, pid) {
		t.Fatalf("background child %d was not running before cleanup", pid)
	}
	if err := KillCommandTree(cmd); err != nil {
		t.Fatalf("KillCommandTree after parent exit: %v", err)
	}
	if processRunningPID(t, pid) {
		t.Fatalf("owned background child %d survived cleanup", pid)
	}
}

func TestCancelAfterParentExitReapsBackgroundChild(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "termllm-cancel-after-exit.exe")
	copyOwnedPing(t, helper)
	pidFile := filepath.Join(dir, "child.pid")
	script := `$p=Start-Process -FilePath '` + helper + `' -ArgumentList '-n','60','127.0.0.1' -WindowStyle Hidden -PassThru; [IO.File]::WriteAllText('` + pidFile + `',[string]$p.Id)`

	cmd := exec.CommandContext(context.Background(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	ConfigureDetachedCommand(cmd)
	if err := RunCommand(cmd); err != nil {
		t.Fatalf("parent run: %v", err)
	}
	pid := readPID(t, pidFile)
	defer exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
	if err := cmd.Cancel(); err != nil {
		t.Fatalf("cancel after parent exit: %v", err)
	}
	if processRunningPID(t, pid) {
		t.Fatalf("owned background child %d survived cancel-after-exit", pid)
	}
}

func TestConcurrentWaitAndDoubleCleanupAreSafe(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "ping -n 30 127.0.0.1")
	ConfigureDetachedCommand(cmd)
	if err := StartCommand(cmd); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	cleanupDone := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_ = KillCommandTree(cmd)
			cleanupDone <- struct{}{}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-cleanupDone:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent cleanup hung")
		}
	}
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait hung after concurrent cleanup")
	}
	if err := KillCommandTree(cmd); err != nil {
		t.Fatalf("double cleanup: %v", err)
	}
	if lookupCommandTree(cmd) != nil {
		t.Fatal("cleanup leaked the command-tree registry entry")
	}
}

func TestContextCancellationReapsTree(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "termllm-cancel-child.exe")
	copyOwnedPing(t, helper)
	pidFile := filepath.Join(dir, "child.pid")
	script := `$p=Start-Process -FilePath '` + helper + `' -ArgumentList '-n','60','127.0.0.1' -WindowStyle Hidden -PassThru; [IO.File]::WriteAllText('` + pidFile + `',[string]$p.Id); Start-Sleep -Seconds 60`

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	ConfigureDetachedCommand(cmd)
	if err := StartCommand(cmd); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	pid := waitPID(t, pidFile)
	defer exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
	cancel()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait hung after context cancellation")
	}
	if processRunningPID(t, pid) {
		t.Fatalf("owned child %d survived context cancellation", pid)
	}
	if err := KillCommandTree(cmd); err != nil {
		t.Fatalf("cleanup after cancellation: %v", err)
	}
}

func copyOwnedPing(t *testing.T, dst string) {
	t.Helper()
	src := filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o700); err != nil {
		t.Fatalf("write helper: %v", err)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pid %q: %v", data, err)
	}
	return pid
}

func waitPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return readPID(t, path)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("pid file %s was not written", path)
	return 0
}

func processImageRunning(t *testing.T, image string) bool {
	t.Helper()
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+image, "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		t.Fatalf("tasklist: %v (%s)", err, out)
	}
	return bytes.Contains(out, []byte(image))
}

func processRunningPID(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		t.Fatalf("tasklist: %v (%s)", err, out)
	}
	return bytes.Contains(out, []byte(strconv.Itoa(pid)))
}

func TestStartCommandFailureReleasesTree(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), filepath.Join(t.TempDir(), "missing-termllm-command.exe"))
	ConfigureDetachedCommand(cmd)
	if err := StartCommand(cmd); err == nil {
		t.Fatal("StartCommand unexpectedly succeeded")
	}
	if lookupCommandTree(cmd) != nil {
		t.Fatal("failed start leaked command tree")
	}
	if err := KillCommandTree(cmd); err != nil {
		t.Fatalf("cleanup after failed start: %v", err)
	}
}

func TestDelayedStartCommandUsesSynchronousOwnership(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	ConfigureDetachedCommand(cmd)
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if err := RunCommand(cmd); err != nil {
		t.Fatalf("RunCommand after delayed configure: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("delayed safe start took %s", elapsed)
	}
	if err := KillCommandTree(cmd); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestKillCommandTreeReleasesUnstartedConfiguredCommand(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	ConfigureDetachedCommand(cmd)
	if lookupCommandTree(cmd) == nil {
		t.Fatal("configured command missing registry entry")
	}
	if err := KillCommandTree(cmd); err != nil {
		t.Fatalf("release unstarted command: %v", err)
	}
	if lookupCommandTree(cmd) != nil {
		t.Fatal("unstarted command registry entry was not released")
	}
}

func TestStartCommandAssignmentFailureReapsProcess(t *testing.T) {
	oldAssign := assignProcessToJobObject
	assignProcessToJobObject = func(windows.Handle, windows.Handle) error { return errors.New("forced assignment failure") }
	defer func() { assignProcessToJobObject = oldAssign }()

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "ping -n 30 127.0.0.1")
	ConfigureDetachedCommand(cmd)
	err := StartCommand(cmd)
	if err == nil || !strings.Contains(err.Error(), "forced assignment failure") {
		t.Fatalf("StartCommand error = %v, want forced assignment diagnostic", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("post-Start assignment failure did not reap the process")
	}
	if lookupCommandTree(cmd) != nil {
		t.Fatal("post-Start assignment failure leaked the command tree")
	}
}

func TestStartCommandResumeFailureReapsProcess(t *testing.T) {
	oldResume := resumeProcessThread
	resumeProcessThread = func(int) error { return errors.New("forced resume failure") }
	defer func() { resumeProcessThread = oldResume }()

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "ping -n 30 127.0.0.1")
	ConfigureDetachedCommand(cmd)
	err := StartCommand(cmd)
	if err == nil || !strings.Contains(err.Error(), "forced resume failure") {
		t.Fatalf("StartCommand error = %v, want forced resume diagnostic", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("post-Start resume failure did not reap the process")
	}
	if lookupCommandTree(cmd) != nil {
		t.Fatal("post-Start resume failure leaked the command tree")
	}
}
