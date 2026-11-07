//go:build !windows

package widgets

import (
	"os"
	"syscall"
)

func killProcessGroup(proc *os.Process, sig syscall.Signal) {
	if proc == nil {
		return
	}
	if err := syscall.Kill(-proc.Pid, sig); err != nil {
		_ = proc.Signal(sig)
	}
}
