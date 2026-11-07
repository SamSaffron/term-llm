//go:build windows

package widgets

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// killProcessGroup terminates a widget process tree. Windows has no POSIX
// signals or process groups; taskkill /T walks the live parent/child tree. The
// process handle in proc remains open until Wait, so its PID cannot be reused
// by an unrelated process while this call runs.
func killProcessGroup(proc *os.Process, sig syscall.Signal) {
	if proc == nil {
		return
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(proc.Pid)).Run(); err != nil {
		_ = proc.Signal(sig)
	}
}
