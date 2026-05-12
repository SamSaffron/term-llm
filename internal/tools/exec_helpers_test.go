package tools

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestResolveToolPath_TildeExpanded verifies that resolveToolPath correctly
// expands a bare ~ to the user's home directory.
func TestResolveToolPath_TildeExpanded(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	got, err := resolveToolPath("~", false)
	if err != nil {
		t.Fatalf("resolveToolPath(~) returned error: %v", err)
	}
	if !strings.HasPrefix(got, home) {
		t.Errorf("resolveToolPath(~) = %q, expected prefix %q", got, home)
	}
}

// TestResolveToolPath_TildeSlashExpanded verifies ~/subpath expansion.
func TestResolveToolPath_TildeSlashExpanded(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	// Use a subpath that is guaranteed to exist so canonicalizePath doesn't fail.
	got, err := resolveToolPath("~/", false)
	if err != nil {
		t.Fatalf("resolveToolPath(~/) returned error: %v", err)
	}
	if !strings.HasPrefix(got, home) {
		t.Errorf("resolveToolPath(~/) = %q, expected prefix %q", got, home)
	}
}

func TestPrepareToolCommand_SkipsNonceScanWithoutEscapedDescendants(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	var calls atomic.Int32
	orig := killTaggedDescendantsFunc
	killTaggedDescendantsFunc = func(string) {
		calls.Add(1)
	}
	defer func() {
		killTaggedDescendantsFunc = orig
	}()

	cmd := exec.CommandContext(context.Background(), "bash", "-lc", "printf ok")
	cleanup, err := prepareToolCommand(cmd)
	if err != nil {
		t.Fatalf("prepareToolCommand error: %v", err)
	}

	if err := cmd.Run(); err != nil {
		t.Fatalf("cmd.Run error: %v", err)
	}
	cleanup()

	if got := calls.Load(); got != 0 {
		t.Fatalf("killTaggedDescendants called %d times, want 0", got)
	}
}

func TestPrepareToolCommand_ScansNonceOnCancellation(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	var calls atomic.Int32
	orig := killTaggedDescendantsFunc
	killTaggedDescendantsFunc = func(string) {
		calls.Add(1)
	}
	defer func() {
		killTaggedDescendantsFunc = orig
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-lc", "sleep 5")
	cleanup, err := prepareToolCommand(cmd)
	if err != nil {
		t.Fatalf("prepareToolCommand error: %v", err)
	}

	if err := cmd.Run(); err == nil {
		t.Fatal("cmd.Run succeeded, want cancellation error")
	}
	cleanup()

	if ctx.Err() == nil {
		t.Fatal("context was not cancelled")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("killTaggedDescendants called %d times, want 1", got)
	}
}
