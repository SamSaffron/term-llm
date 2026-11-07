//go:build !windows

package widgets

import (
	"os"
	"os/exec"
	"syscall"
)

func killProcessGroup(proc *os.Process, _ *exec.Cmd, sig syscall.Signal) {
	if proc == nil {
		return
	}
	if err := syscall.Kill(-proc.Pid, sig); err != nil {
		_ = proc.Signal(sig)
	}
}
