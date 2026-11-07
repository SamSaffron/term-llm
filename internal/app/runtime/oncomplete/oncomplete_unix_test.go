//go:build !windows

package oncomplete

import "testing"

func TestRunCaptureUsesPOSIXShDespiteFishShell(t *testing.T) {
	t.Setenv("SHELL", "/bin/fish")
	result, err := RunCapture("printf 'posix-hook'", "")
	if err != nil {
		t.Fatalf("RunCapture: %v", err)
	}
	if result.Stdout != "posix-hook" {
		t.Fatalf("stdout = %q, want posix-hook", result.Stdout)
	}
}
