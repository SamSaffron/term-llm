//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/shellcmd"
)

func TestResolveFileWindowsPathForms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(path, []byte("  secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	forms := []string{"file://" + path, "file:///" + filepath.ToSlash(path)}
	for _, form := range forms {
		got, err := ResolveValue(form)
		if err != nil {
			t.Fatalf("ResolveValue(%q): %v", form, err)
		}
		if got != "secret-token" {
			t.Fatalf("ResolveValue(%q) = %q", form, got)
		}
	}
}

func TestResolveFileWindowsFragment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(path, []byte(`{"access_token":"tok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveValue("file://" + path + "#access_token")
	if err != nil {
		t.Fatalf("ResolveValue: %v", err)
	}
	if got != "tok" {
		t.Fatalf("fragment value = %q", got)
	}
}

func TestResolveCommandUsesDefaultPowerShell(t *testing.T) {
	t.Setenv("SHELL", "")
	base := strings.ToLower(filepath.Base(shellcmd.Path()))
	if base != "powershell.exe" && base != "pwsh.exe" {
		t.Skipf("default PowerShell unavailable: %s", base)
	}
	out, err := resolveCommand("Write-Output 'powershell-resolver-ok'")
	if err != nil {
		t.Fatalf("resolveCommand: %v", err)
	}
	if got := strings.TrimSpace(out); got != "powershell-resolver-ok" {
		t.Fatalf("resolveCommand output = %q", got)
	}
}
