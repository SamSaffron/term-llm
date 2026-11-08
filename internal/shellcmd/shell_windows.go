//go:build windows

package shellcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Path selects the shell. An explicit SHELL is honored (Windows OpenSSH sets
// it to PowerShell); otherwise prefer PowerShell 7, Windows PowerShell, then
// COMSPEC.
func Path() string {
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	for _, name := range []string{"pwsh.exe", "powershell.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	if comspec := strings.TrimSpace(os.Getenv("COMSPEC")); comspec != "" {
		return comspec
	}
	return "cmd.exe"
}

// Args returns the argv used to run command in shellPath. Windows shells do
// not share the POSIX -c convention.
func Args(shellPath, command string) []string {
	switch strings.ToLower(filepath.Base(shellPath)) {
	case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command}
	case "cmd", "cmd.exe":
		return []string{"/d", "/s", "/c", command}
	default:
		// An explicit SHELL pointing at bash/sh/zsh or another POSIX shell.
		return []string{"-c", command}
	}
}

// Invocation returns the shell and argv for a non-interactive command.
func Invocation(command string) (string, []string) {
	path := Path()
	return path, Args(path, command)
}

// HookInvocation uses native shell selection on Windows, where no POSIX sh
// contract exists.
func HookInvocation(command string) (string, []string) {
	return Invocation(command)
}

// InteractivePath uses native shell selection on Windows.
func InteractivePath() string {
	return Path()
}
