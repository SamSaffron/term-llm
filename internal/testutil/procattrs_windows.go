//go:build windows

package testutil

import (
	"syscall"

	"github.com/samsaffron/term-llm/internal/testutil/processinfo"
)

func ProcessIsDetached(attr *syscall.SysProcAttr) bool {
	return processinfo.ProcessIsDetached(attr)
}

func ProcessIsProcessGroup(attr *syscall.SysProcAttr) bool {
	return processinfo.ProcessIsProcessGroup(attr)
}

func KillProcess(pid int) error { return processinfo.KillProcess(pid) }
