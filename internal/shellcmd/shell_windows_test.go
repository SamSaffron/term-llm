//go:build windows

package shellcmd

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestArgsSelectsWindowsShellSyntax(t *testing.T) {
	tests := []struct {
		shell string
		args  []string
	}{
		{shell: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, args: []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "echo hi"}},
		{shell: `C:\Program Files\PowerShell\7\pwsh.exe`, args: []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "echo hi"}},
		{shell: `C:\Windows\System32\cmd.exe`, args: []string{"/d", "/s", "/c", "echo hi"}},
		{shell: `C:\Program Files\Git\bin\bash.exe`, args: []string{"-c", "echo hi"}},
	}
	for _, tc := range tests {
		if got := Args(tc.shell, "echo hi"); !reflect.DeepEqual(got, tc.args) {
			t.Errorf("Args(%q) = %v, want %v", tc.shell, got, tc.args)
		}
	}
}

func TestPathHonorsExplicitShell(t *testing.T) {
	t.Setenv("SHELL", `C:\tools\custom.exe`)
	if got := Path(); got != `C:\tools\custom.exe` {
		t.Fatalf("Path = %q", got)
	}
}

func TestPathFallsBackToPowerShell(t *testing.T) {
	t.Setenv("SHELL", "")
	got := Path()
	if got == "" {
		t.Fatal("Path returned empty")
	}
	if base := filepath.Base(strings.ToLower(got)); base != "powershell.exe" && base != "pwsh.exe" && base != "cmd.exe" {
		t.Fatalf("Path = %q, want a PowerShell or cmd fallback", got)
	}
}
