package tools

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestShellTool_RunawayOutputStopsAtHardLimit(t *testing.T) {
	if _, err := exec.LookPath("yes"); err != nil {
		t.Skip("yes not available")
	}

	limits := DefaultOutputLimits()
	limits.MaxBytes = 32
	limits.CumulativeHard = 256
	tool := NewShellTool(nil, nil, limits)
	args := mustMarshalShellArgs(ShellArgs{Command: "yes x", TimeoutSeconds: 5})

	start := time.Now()
	output, err := tool.Execute(context.Background(), args)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("runaway output should stop at hard output limit quickly; elapsed=%s output=%s", elapsed, output.Content)
	}
	if output.TimedOut {
		t.Fatalf("output-limit cancellation should not be reported as timeout; output=%s", output.Content)
	}
	if !strings.Contains(output.Content, "[Command stopped after exceeding output limit]") {
		t.Fatalf("expected output-limit marker, got: %s", output.Content)
	}
	if !strings.Contains(output.Content, "[Output truncated due to size limit]") {
		t.Fatalf("expected truncation marker, got: %s", output.Content)
	}
}

func BenchmarkShellToolRunawayOutputSmallLimit(b *testing.B) {
	if _, err := exec.LookPath("yes"); err != nil {
		b.Skip("yes not available")
	}

	limits := DefaultOutputLimits()
	limits.MaxBytes = 16
	limits.CumulativeHard = 256
	tool := NewShellTool(nil, nil, limits)
	args := mustMarshalShellArgs(ShellArgs{Command: "yes x", TimeoutSeconds: 1})

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := tool.Execute(context.Background(), args); err != nil {
			b.Fatal(err)
		}
	}
}
