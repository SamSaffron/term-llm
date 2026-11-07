//go:build !windows

package config

import "testing"

func TestResolveCommandUsesPOSIXShDespiteFishShell(t *testing.T) {
	t.Setenv("SHELL", "/bin/fish")
	out, err := resolveCommand("printf 'posix-ok'")
	if err != nil {
		t.Fatalf("resolveCommand: %v", err)
	}
	if out != "posix-ok" {
		t.Fatalf("resolveCommand output = %q, want posix-ok", out)
	}
}
