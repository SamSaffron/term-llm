//go:build !windows

package shellcmd

import (
	"os"
	"strings"
)

// Path returns the user's shell, defaulting to bash.
func Path() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "bash"
}

// Args returns the argv used to run command in shellPath.
func Args(_, command string) []string {
	return []string{"-c", command}
}

// Invocation returns the shell and argv for a non-interactive command.
func Invocation(command string) (string, []string) {
	path := Path()
	return path, Args(path, command)
}

// HookInvocation returns the shell used by established POSIX-only call sites
// (config $(...) expansion and on_complete hooks). Those contracts were always
// `sh -c`; they must not follow the user's interactive SHELL.
func HookInvocation(command string) (string, []string) {
	return "sh", []string{"-c", command}
}

// InteractivePath preserves the console's historical fallback: a trimmed SHELL
// when set, otherwise the POSIX sh.
func InteractivePath() string {
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	return "sh"
}
