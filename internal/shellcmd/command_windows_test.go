//go:build windows

package shellcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdCommandContextPreservesQuotedFilePaths(t *testing.T) {
	comspec := testComspec(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "A B.txt")
	if err := os.WriteFile(file, []byte("hello from quoted path\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := CommandContextFor(context.Background(), comspec, `type "`+file+`"`).CombinedOutput()
	if err != nil {
		t.Fatalf("cmd type: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), "hello from quoted path") {
		t.Fatalf("cmd output = %q", out)
	}
}

func TestCmdCommandContextPreservesEmbeddedQuotesAndRedirection(t *testing.T) {
	comspec := testComspec(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "quoted output.txt")
	command := `echo "quoted & text">"` + target + `"`
	out, err := CommandContextFor(context.Background(), comspec, command).CombinedOutput()
	if err != nil {
		t.Fatalf("cmd redirection: %v (%s)", err, out)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read redirected output: %v", err)
	}
	if !strings.Contains(string(data), "quoted & text") {
		t.Fatalf("redirected output = %q", data)
	}
}

func TestCmdCommandContextRunsQuotedExecutablePath(t *testing.T) {
	comspec := testComspec(t)
	dir := t.TempDir()
	copyPath := filepath.Join(dir, "cmd copy.exe")
	data, err := os.ReadFile(comspec)
	if err != nil {
		t.Fatalf("read cmd.exe: %v", err)
	}
	if err := os.WriteFile(copyPath, data, 0o700); err != nil {
		t.Fatalf("write cmd copy: %v", err)
	}
	command := `"` + copyPath + `" /d /c echo quoted-exe-ok`
	out, err := CommandContextFor(context.Background(), comspec, command).CombinedOutput()
	if err != nil {
		t.Fatalf("quoted executable: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), "quoted-exe-ok") {
		t.Fatalf("quoted executable output = %q", out)
	}
}

func TestDefaultPowerShellCommandContext(t *testing.T) {
	t.Setenv("SHELL", "")
	base := strings.ToLower(filepath.Base(Path()))
	if base != "powershell.exe" && base != "pwsh.exe" {
		t.Skipf("default PowerShell unavailable: %s", base)
	}
	out, err := CommandContext(context.Background(), `Write-Output 'default-shell-ok'`).CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell command: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), "default-shell-ok") {
		t.Fatalf("PowerShell output = %q", out)
	}
}

func testComspec(t *testing.T) string {
	t.Helper()
	if comspec := strings.TrimSpace(os.Getenv("COMSPEC")); comspec != "" {
		return comspec
	}
	path, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Skipf("cmd.exe unavailable: %v", err)
	}
	return path
}
