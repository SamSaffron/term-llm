package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/testutil"
)

func testProgramJob(t *testing.T, cfg ProgramConfig) Job {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return Job{ID: "job-program-test", Name: "program-test", RunnerType: RunnerProgram, RunnerConfig: raw}
}

func TestProgramRunnerShellArgsBecomePositionalParameters(t *testing.T) {
	runner := &ProgramRunner{}
	job := testProgramJob(t, ProgramConfig{
		Command: `printf '%s %s' "$1" "$2"`,
		Args:    []string{"alpha", "beta"},
		Shell:   true,
	})
	result, err := runner.Run(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "alpha beta" {
		t.Fatalf("stdout = %q, want alpha beta", result.Stdout)
	}
}

func TestProgramRunnerTimeoutKillsBackgroundChildrenPromptly(t *testing.T) {
	runner := &ProgramRunner{}
	job := testProgramJob(t, ProgramConfig{Command: `sleep 1 & wait`, Shell: true})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runner.Run(ctx, job, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 750*time.Millisecond {
		t.Fatalf("Run took too long after timeout: %s", time.Since(start))
	}
}

func TestProgramRunnerTruncatesCapturedOutput(t *testing.T) {
	runner := &ProgramRunner{OutputLimit: 32}
	job := testProgramJob(t, ProgramConfig{
		Command: `i=0; while [ $i -lt 200 ]; do printf x; i=$((i+1)); done`,
		Shell:   true,
	})
	result, err := runner.Run(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Truncated {
		t.Fatal("expected truncated result")
	}
	if len(result.Stdout) > int(runner.OutputLimit) {
		t.Fatalf("stdout length = %d, want <= %d", len(result.Stdout), runner.OutputLimit)
	}
	exitReason, truncated := ClassifyRunError(nil, result)
	if exitReason != ExitReasonNatural {
		t.Fatalf("exitReason = %q, want %q", exitReason, ExitReasonNatural)
	}
	if !truncated {
		t.Fatalf("expected ClassifyRunError to preserve truncation")
	}
}

func TestProgramRunnerDoesNotLeakBackgroundChildren(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	runner := &ProgramRunner{}
	job := testProgramJob(t, ProgramConfig{
		Command: fmt.Sprintf(`sleep 30 >/dev/null 2>&1 & echo $! > %s`, shellQuote(pidPath)),
		Shell:   true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := runner.Run(ctx, job, nil)
	if err != nil {
		t.Fatalf("Run: %v (stdout=%q stderr=%q)", err, result.Stdout, result.Stderr)
	}
	pidRaw, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidRaw)))
	if err != nil {
		t.Fatalf("parse child pid %q: %v", string(pidRaw), err)
	}
	testutil.WaitForProcessExit(t, pid, time.Second)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
