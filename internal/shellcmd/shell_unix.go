//go:build !windows

package shellcmd

import "os"

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
