package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestShellTool_BackgroundedChildKilled pins down the session-1708 behaviour:
// an LLM-issued `nohup foo &` must not leave an orphan process alive after
// the shell tool returns. The tool call is cancelled mid-flight and the
// grandchild must be reaped regardless.
func TestShellTool_BackgroundedChildKilled(t *testing.T) {
	sentinel := uniqueSentinel(t, "bg")
	logPath := fmt.Sprintf("/tmp/%s.log", sentinel)
	defer os.Remove(logPath)

	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	cmd := fmt.Sprintf(
		"nohup bash -c 'sleep 120; :%s' >%s 2>&1 & echo pid=$! && sleep 0.1",
		sentinel, logPath,
	)
	runAndAssertReaped(t, tool, cmd, sentinel, true /* cancelMidway */)
}

// TestShellTool_SetsidDescendantKilled covers the nastier case: a descendant
// that detaches from the process group via `setsid`. The pgroup kill can't
// reach it, so cleanup falls back to /proc env scanning by nonce.
func TestShellTool_SetsidDescendantKilled(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available on this platform")
	}
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("no /proc — nonce-based descendant reap is Linux-only")
	}

	sentinel := uniqueSentinel(t, "setsid")
	logPath := fmt.Sprintf("/tmp/%s.log", sentinel)
	defer os.Remove(logPath)

	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	// setsid detaches the child into its own session + pgroup, so our
	// first-pass SIGKILL -pgid can't find it. The nonce scan is what saves us.
	cmd := fmt.Sprintf(
		"setsid bash -c 'sleep 120; :%s' >%s 2>&1 < /dev/null & echo pid=$! && sleep 0.1",
		sentinel, logPath,
	)
	runAndAssertReaped(t, tool, cmd, sentinel, false /* natural completion */)
}

func uniqueSentinel(t *testing.T, tag string) string {
	t.Helper()
	return fmt.Sprintf("term-llm-leak-%s-%d-%d", tag, os.Getpid(), time.Now().UnixNano())
}

func runAndAssertReaped(t *testing.T, tool *ShellTool, command, sentinel string, cancelMidway bool) {
	t.Helper()
	args := mustMarshalShellArgs(ShellArgs{
		Command:        command,
		TimeoutSeconds: 5,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	var output string
	go func() {
		out, err := tool.Execute(ctx, args)
		if err != nil {
			t.Errorf("unexpected err: %v", err)
		}
		output = out.Content
		close(done)
	}()

	if cancelMidway {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = exec.Command("pkill", "-f", sentinel).Run()
		t.Fatal("shell Execute did not return within 10s")
	}

	// Give cleanup a moment to complete.
	time.Sleep(250 * time.Millisecond)

	found, _ := exec.Command("pgrep", "-f", sentinel).Output()
	stray := strings.TrimSpace(string(found))
	if stray != "" {
		_ = exec.Command("pkill", "-f", sentinel).Run()
		t.Fatalf("descendant sentinel process still alive after shell returned:\n  pgrep -f %s -> %q\n  tool output: %s",
			sentinel, stray, output)
	}
}
