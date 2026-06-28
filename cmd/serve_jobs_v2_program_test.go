package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testJobsV2ProgramJob(t *testing.T, cfg jobsV2ProgramConfig) jobsV2Job {
	t.Helper()

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	return jobsV2Job{RunnerConfig: raw}
}

func TestJobsV2ProgramRunner_ShellArgsBecomePositionalParameters(t *testing.T) {
	runner := &jobsV2ProgramRunner{}
	job := testJobsV2ProgramJob(t, jobsV2ProgramConfig{
		Command: `printf '%s %s' "$1" "$2"`,
		Args:    []string{"alpha", "beta"},
		Shell:   true,
	})

	result, err := runner.Run(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if got := result.Stdout; got != "alpha beta" {
		t.Fatalf("stdout = %q, want %q", got, "alpha beta")
	}
}

func TestJobsV2ProgramRunner_TimeoutKillsBackgroundChildrenPromptly(t *testing.T) {
	runner := &jobsV2ProgramRunner{}
	job := testJobsV2ProgramJob(t, jobsV2ProgramConfig{
		Command: `sleep 1 & wait`,
		Shell:   true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _ = runner.Run(ctx, job, nil)
	elapsed := time.Since(start)

	if elapsed > 600*time.Millisecond {
		t.Fatalf("Run took %v after timeout, want < 600ms", elapsed)
	}
}

func TestJobsV2ProgramRunner_CleansUpBackgroundChildOnSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cleanup test is unix-specific")
	}

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	runner := &jobsV2ProgramRunner{}
	job := testJobsV2ProgramJob(t, jobsV2ProgramConfig{
		Command: fmt.Sprintf(`sleep 30 >/dev/null 2>&1 & echo $! > %s`, jobsV2ProgramTestShellQuote(pidPath)),
		Shell:   true,
	})

	result, err := runner.Run(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("Run failed: %v (stdout=%q stderr=%q)", err, result.Stdout, result.Stderr)
	}

	pid := readJobsV2ProgramPID(t, pidPath)
	defer func() {
		if !jobsV2ProcessHasExited(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}()

	waitForJobsV2ProcessExit(t, pid)
}

func TestJobsV2ProgramRunner_TruncatesCapturedOutput(t *testing.T) {
	origLimit := jobsV2ProgramOutputLimit
	jobsV2ProgramOutputLimit = 32
	defer func() {
		jobsV2ProgramOutputLimit = origLimit
	}()

	runner := &jobsV2ProgramRunner{}
	job := testJobsV2ProgramJob(t, jobsV2ProgramConfig{
		Command: `i=0; while [ $i -lt 200 ]; do printf x; i=$((i+1)); done`,
		Shell:   true,
	})

	result, err := runner.Run(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if got := len(result.Stdout); got != 32 {
		t.Fatalf("stdout length = %d, want %d", got, 32)
	}
	if !result.Truncated {
		t.Fatalf("expected result to be marked truncated")
	}

	exitReason, truncated := classifyRunError(nil, result)
	if exitReason != exitReasonNatural {
		t.Fatalf("exitReason = %q, want %q", exitReason, exitReasonNatural)
	}
	if !truncated {
		t.Fatalf("expected classifyRunError to preserve truncation")
	}
}

func readJobsV2ProgramPID(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read child pid file: %v", err)
	}
	pidText := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatalf("parse child pid %q: %v", pidText, err)
	}
	return pid
}

func waitForJobsV2ProcessExit(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if jobsV2ProcessHasExited(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for background child process %d to exit", pid)
}

func jobsV2ProcessHasExited(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	if runtime.GOOS == "linux" {
		state, ok := jobsV2LinuxProcState(pid)
		return ok && state == 'Z'
	}
	return false
}

func jobsV2LinuxProcState(pid int) (byte, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	stat := string(data)
	end := strings.LastIndex(stat, ")")
	if end == -1 {
		return 0, false
	}
	rest := strings.TrimSpace(stat[end+1:])
	if rest == "" {
		return 0, false
	}
	return rest[0], true
}

func jobsV2ProgramTestShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
