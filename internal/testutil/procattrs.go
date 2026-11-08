package testutil

import (
	"syscall"

	"github.com/samsaffron/term-llm/internal/testutil/processinfo"
)

// ProcessIsDetached reports whether attr starts the child in a new session or
// process group.
func ProcessIsDetached(attr *syscall.SysProcAttr) bool {
	return processinfo.ProcessIsDetached(attr)
}

// ProcessIsProcessGroup reports whether attr starts the child in a new process
// group without detaching its controlling terminal.
func ProcessIsProcessGroup(attr *syscall.SysProcAttr) bool {
	return processinfo.ProcessIsProcessGroup(attr)
}

// KillProcess forcefully terminates pid.
func KillProcess(pid int) error { return processinfo.KillProcess(pid) }
