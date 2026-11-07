//go:build !windows

package testutil

import "syscall"

// ProcessIsDetached reports whether attr starts the child in a new session.
func ProcessIsDetached(attr *syscall.SysProcAttr) bool {
	return attr != nil && attr.Setsid
}

// ProcessIsProcessGroup reports whether attr starts the child in a new process
// group without detaching its controlling terminal.
func ProcessIsProcessGroup(attr *syscall.SysProcAttr) bool {
	return attr != nil && attr.Setpgid
}

// KillProcess forcefully terminates pid.
func KillProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
