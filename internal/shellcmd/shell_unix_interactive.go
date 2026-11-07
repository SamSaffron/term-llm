//go:build !windows

package shellcmd

// IsPowerShell reports whether shellPath is a PowerShell executable.
func IsPowerShell(shellPath string) bool { return false }
