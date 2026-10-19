package oncomplete

import (
	"testing"
	"time"
)

func TestRunOnCompleteCapture_CapturesStdout(t *testing.T) {
	result, err := RunCapture("cat", "hello")
	if err != nil {
		t.Fatalf("RunOnCompleteCapture error: %v", err)
	}
	if result.Stdout != "hello" {
		t.Errorf("stdout = %q, want %q", result.Stdout, "hello")
	}
	if result.Stderr != "" {
		t.Errorf("stderr = %q, want empty", result.Stderr)
	}
}

func TestRunOnCompleteCapture_CapturesStderrAndError(t *testing.T) {
	result, err := RunCapture("echo err >&2; exit 7", "")
	if err == nil {
		t.Fatal("expected command error, got nil")
	}
	if result.Stdout != "" {
		t.Errorf("stdout = %q, want empty", result.Stdout)
	}
	if result.Stderr != "err\n" {
		t.Errorf("stderr = %q, want %q", result.Stderr, "err\n")
	}
}

func TestRunOnCompleteCapture_TimesOutHungHook(t *testing.T) {
	oldTimeout := onCompleteTimeout
	onCompleteTimeout = 100 * time.Millisecond
	t.Cleanup(func() { onCompleteTimeout = oldTimeout })

	started := time.Now()
	_, err := RunCapture("sleep 10", "")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if err.Error() != "timed out after 100ms" {
		t.Fatalf("error = %q, want %q", err.Error(), "timed out after 100ms")
	}
	if elapsed >= time.Second {
		t.Fatalf("RunOnCompleteCapture took %v, want timeout well under 1s", elapsed)
	}
}

func TestRunOnCompleteCapture_ReturnsWhenBackgroundChildKeepsPipeOpen(t *testing.T) {
	oldTimeout := onCompleteTimeout
	oldWaitDelay := onCompleteWaitDelay
	onCompleteTimeout = time.Second
	onCompleteWaitDelay = 50 * time.Millisecond
	t.Cleanup(func() {
		onCompleteTimeout = oldTimeout
		onCompleteWaitDelay = oldWaitDelay
	})

	started := time.Now()
	result, err := RunCapture("(sleep 10) & printf done", "")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if result.Stdout != "done" {
		t.Fatalf("stdout = %q, want %q", result.Stdout, "done")
	}
	if elapsed >= time.Second {
		t.Fatalf("RunOnCompleteCapture took %v, want wait-delay recovery well under 1s", elapsed)
	}
}
