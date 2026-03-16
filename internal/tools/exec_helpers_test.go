package tools

import (
	"os"
	"strings"
	"testing"
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
