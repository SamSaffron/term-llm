//go:build !windows

package chat

import "testing"

func TestInteractiveShellPathPreservesUnixFallback(t *testing.T) {
	t.Setenv("SHELL", " /bin/fish ")
	if got := interactiveShellPath(); got != "/bin/fish" {
		t.Fatalf("interactiveShellPath = %q, want /bin/fish", got)
	}
	t.Setenv("SHELL", "")
	if got := interactiveShellPath(); got != "sh" {
		t.Fatalf("interactiveShellPath fallback = %q, want sh", got)
	}
}
