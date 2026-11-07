//go:build windows

package processinfo

import (
	"os/exec"
	"strconv"
	"syscall"
)

const createNewProcessGroup = 0x00000200

// ProcessIsDetached reports whether attr starts the child in a new process
// group. Windows has no sessions; a new process group is the closest mapping.
func ProcessIsDetached(attr *syscall.SysProcAttr) bool {
	return attr != nil && attr.CreationFlags&createNewProcessGroup != 0
}

// ProcessIsProcessGroup reports whether attr starts the child in a new process
// group.
func ProcessIsProcessGroup(attr *syscall.SysProcAttr) bool {
	return ProcessIsDetached(attr)
}

// KillProcess forcefully terminates pid and its descendants.
func KillProcess(pid int) error {
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
