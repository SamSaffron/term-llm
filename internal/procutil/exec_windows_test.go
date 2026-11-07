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

func mustPrepare(t *testing.T, cmd *exec.Cmd) *Command {
	t.Helper()
	owned, err := PrepareCommand(cmd)
	if err != nil {
		t.Fatalf("PrepareCommand: %v", err)
	}
	return owned
}

func TestPrepareDetachedCommandCreatesProcessGroupWithoutWindow(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	owned := mustPrepare(t, cmd)
	defer owned.Close()
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&createNewProcessGroup == 0 {
		t.Fatalf("CreationFlags = %#x, want CREATE_NEW_PROCESS_GROUP", cmd.SysProcAttr.CreationFlags)
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("CreationFlags = %#x, want CREATE_NO_WINDOW", cmd.SysProcAttr.CreationFlags)
	}
	if cmd.Cancel == nil {
		t.Fatal("expected process-tree cancellation hook")
	}
}

func TestPrepareProcessGroupKeepsConsole(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	owned, err := PrepareCommandProcessGroup(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&createNewProcessGroup == 0 {
		t.Fatalf("SysProcAttr = %#v, want CREATE_NEW_PROCESS_GROUP", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow != 0 {
		t.Fatal("credential-capable process group must keep the caller's console")
	}
}

func TestPrepareLeavesPlainCommandCancelNil(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	owned := mustPrepare(t, cmd)
	defer owned.Close()
	if cmd.Cancel != nil {
		t.Fatal("plain exec.Command must not receive a Cancel hook")
	}
}

func TestPrepareRejectsNilCommand(t *testing.T) {
	if _, err := PrepareCommand(nil); err == nil {
		t.Fatal("PrepareCommand(nil) succeeded")
	}
}

func TestRunCommandResumesSuspendedProcess(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 7")
	owned := mustPrepare(t, cmd)
	defer owned.Close()
	err := owned.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("Run error = %v, want exit status 7", err)
	}
}

func TestCloseTerminatesChild(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "ping -n 30 127.0.0.1")
	owned := mustPrepare(t, cmd)
	if err := owned.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = owned.Wait(); close(done) }()
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process tree survived Close")
	}
}

func TestCloseAfterParentExitReapsBackgroundChild(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "termllm-parent-exit-child.exe")
	copyOwnedPing(t, helper)
	pidFile := filepath.Join(dir, "child.pid")
	script := `$p=Start-Process -FilePath '` + helper + `' -ArgumentList '-n','60','127.0.0.1' -WindowStyle Hidden -PassThru; [IO.File]::WriteAllText('` + pidFile + `',[string]$p.Id)`

	cmd := exec.CommandContext(context.Background(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	owned := mustPrepare(t, cmd)
	defer owned.Close()
	if err := owned.Run(); err != nil {
		t.Fatal(err)
	}
	pid := readPID(t, pidFile)
	defer exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
	if !processRunningPID(t, pid) {
		t.Fatalf("background child %d was not running before cleanup", pid)
	}
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	if processRunningPID(t, pid) {
		t.Fatalf("owned background child %d survived Close", pid)
	}
}

func TestConcurrentWaitAndDoubleCloseAreSafe(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "ping -n 30 127.0.0.1")
	owned := mustPrepare(t, cmd)
	if err := owned.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- owned.Wait() }()
	cleanupDone := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_ = owned.Close()
			cleanupDone <- struct{}{}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-cleanupDone:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Close hung")
		}
	}
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait hung after Close")
	}
	if err := owned.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
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
	owned := mustPrepare(t, cmd)
	defer owned.Close()
	if err := owned.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- owned.Wait() }()
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
}

func TestDelayedStartUsesSynchronousOwnership(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	owned := mustPrepare(t, cmd)
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if err := owned.Run(); err != nil {
		t.Fatalf("Run after delayed prepare: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("delayed safe start took %s", elapsed)
	}
}

func TestCloseBeforeStartCreatesNoProcess(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	owned := mustPrepare(t, cmd)
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owned.Start(); !errors.Is(err, ErrCommandClosed) {
		t.Fatalf("Start after Close = %v, want ErrCommandClosed", err)
	}
	if cmd.Process != nil {
		t.Fatal("Start after Close created an OS process")
	}
}

func TestCloseBetweenCreationAndAssignmentReapsSuspendedProcess(t *testing.T) {
	oldBefore := beforeAssignProcess
	entered := make(chan struct{})
	release := make(chan struct{})
	beforeAssignProcess = func() {
		close(entered)
		<-release
	}
	defer func() { beforeAssignProcess = oldBefore }()

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	owned := mustPrepare(t, cmd)
	startDone := make(chan error, 1)
	go func() { startDone <- owned.Start() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("assignment was not reached")
	}
	closeDone := make(chan struct{})
	go func() {
		_ = owned.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung between process creation and assignment")
	}
	close(release)
	select {
	case err := <-startDone:
		if err == nil || !strings.Contains(err.Error(), "job was closed before assignment") {
			t.Fatalf("Start error = %v, want closed-job diagnostic", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start hung after Close between creation and assignment")
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("Start did not reap the suspended process")
	}
}

func TestConcurrentStartAndCloseDoesNotHang(t *testing.T) {
	for i := 0; i < 10; i++ {
		cmd := exec.CommandContext(context.Background(), "cmd", "/c", "ping -n 30 127.0.0.1")
		owned := mustPrepare(t, cmd)
		startDone := make(chan error, 1)
		go func() { startDone <- owned.Start() }()
		closeDone := make(chan struct{})
		go func() {
			_ = owned.Close()
			close(closeDone)
		}()
		startErr := <-startDone
		select {
		case <-closeDone:
		case <-time.After(5 * time.Second):
			t.Fatal("Close hung during Start")
		}
		if startErr == nil {
			_ = owned.Wait()
		}
	}
}

func TestCloseReleasesUnstartedCommand(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit 0")
	owned := mustPrepare(t, cmd)
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owned.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func TestStartCommandAssignmentFailureReapsProcess(t *testing.T) {
	oldAssign := assignProcessToJobObject
	assignProcessToJobObject = func(windows.Handle, windows.Handle) error { return errors.New("forced assignment failure") }
	defer func() { assignProcessToJobObject = oldAssign }()

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "ping -n 30 127.0.0.1")
	owned := mustPrepare(t, cmd)
	err := owned.Start()
	if err == nil || !strings.Contains(err.Error(), "forced assignment failure") {
		t.Fatalf("Start error = %v, want forced assignment diagnostic", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("post-Start assignment failure did not reap the process")
	}
}

func TestStartCommandResumeFailureReapsProcess(t *testing.T) {
	oldResume := resumeProcessThread
	resumeProcessThread = func(int) error { return errors.New("forced resume failure") }
	defer func() { resumeProcessThread = oldResume }()

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "ping -n 30 127.0.0.1")
	owned := mustPrepare(t, cmd)
	err := owned.Start()
	if err == nil || !strings.Contains(err.Error(), "forced resume failure") {
		t.Fatalf("Start error = %v, want forced resume diagnostic", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("post-Start resume failure did not reap the process")
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

func processRunningPID(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		t.Fatalf("tasklist: %v (%s)", err, out)
	}
	return bytes.Contains(out, []byte(strconv.Itoa(pid)))
}
