package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/llm"
)

// TestShellTool_BackgroundedChildKilled pins down the session-1708 behaviour:
// an LLM-issued `nohup foo &` must not leave an orphan process alive after
// the shell tool returns. The tool call is cancelled mid-flight and the
// grandchild must be reaped regardless.
func TestShellTool_BackgroundedChildKilled(t *testing.T) {
	t.Parallel()

	sentinel := uniqueSentinel(t, "bg")
	logPath := fmt.Sprintf("/tmp/%s.log", sentinel)
	defer os.Remove(logPath)

	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	// The trailing sleep keeps the foreground shell alive well past the
	// cancellation point so the cancel always lands mid-flight.
	cmd := fmt.Sprintf(
		"nohup bash -c 'sleep 120; :%s' >%s 2>&1 & echo pid=$! && sleep 5",
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
	t.Parallel()

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
		time.Sleep(100 * time.Millisecond)
		cancel()
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = exec.Command("pkill", "-f", sentinel).Run()
		t.Fatal("shell Execute did not return within 10s")
	}

	// Poll until cleanup reaps the descendant; bail out fast on success.
	deadline := time.Now().Add(2 * time.Second)
	var stray string
	for {
		found, _ := exec.Command("pgrep", "-f", sentinel).Output()
		stray = strings.TrimSpace(string(found))
		if stray == "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stray != "" {
		_ = exec.Command("pkill", "-f", sentinel).Run()
		t.Fatalf("descendant sentinel process still alive after shell returned:\n  pgrep -f %s -> %q\n  tool output: %s",
			sentinel, stray, output)
	}
}

type blockingAttributedRecorder struct {
	*fakeFileRecorder
	recordStarted chan filetrack.ChangeRecord
	release       chan struct{}
}

func (r *blockingAttributedRecorder) RecordAttributedChange(ctx context.Context, rec filetrack.ChangeRecord) (*llm.FileChange, error) {
	select {
	case r.recordStarted <- rec:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.fakeFileRecorder.RecordAttributedChange(ctx, rec)
}

func TestShellToolCleansBackgroundDescendantsBeforeTracking(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-group cleanup regression is Linux-specific")
	}

	dir := t.TempDir()
	trackedPath := filepath.Join(dir, "tracked.txt")
	childReleasePath := filepath.Join(dir, "child-release")
	if err := os.WriteFile(trackedPath, []byte("before\n"), 0644); err != nil {
		t.Fatal(err)
	}

	recorder := &blockingAttributedRecorder{
		fakeFileRecorder: &fakeFileRecorder{},
		recordStarted:    make(chan filetrack.ChangeRecord, 1),
		release:          make(chan struct{}),
	}
	recorderReleased := false
	defer func() {
		if !recorderReleased {
			close(recorder.release)
		}
	}()

	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.recorder = recorder
	command := "(printf ready > child-ready; while [ ! -e child-release ]; do sleep 0.01; done; printf 'second\\n' > tracked.txt) </dev/null >/dev/null 2>&1 & " +
		"while [ ! -e child-ready ]; do sleep 0.01; done; printf 'initial\\n' > tracked.txt"
	args := mustMarshalShellArgs(ShellArgs{
		Command:    command,
		WorkingDir: dir,
		OutputClaims: []OutputClaim{{
			Path: "tracked.txt",
			Kind: filetrack.ClaimTransform,
		}},
	})

	done := make(chan error, 1)
	go func() {
		out, err := tool.Execute(trackingContext(), args)
		if err == nil && out.IsError {
			err = fmt.Errorf("shell command failed: %s", out.Content)
		}
		done <- err
	}()

	var rec filetrack.ChangeRecord
	select {
	case rec = <-recorder.recordStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("shell tracking did not reach the recorder")
	}
	if got := string(rec.After); got != "initial\n" {
		t.Fatalf("recorded after-content = %q, want initial value", got)
	}

	// If cleanup is deferred until Execute returns, releasing this known-live
	// child lets it mutate the file while the recorder keeps tracking blocked.
	if err := os.WriteFile(childReleasePath, []byte("release\n"), 0644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	final, err := os.ReadFile(trackedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(final) != string(rec.After) {
		t.Fatalf("file changed after tracking snapshot: recorded %q, final %q", rec.After, final)
	}

	close(recorder.release)
	recorderReleased = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shell Execute did not return after recorder release")
	}
}
