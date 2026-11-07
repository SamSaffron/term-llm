//go:build windows

package widgets

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/samsaffron/term-llm/internal/procutil"
)

// killProcessGroup terminates a widget process tree through its owned Job
// Object. The job handle remains valid after the parent exits and cannot target
// an unrelated process that reused its PID.
func killProcessGroup(proc *os.Process, cmd *exec.Cmd, sig syscall.Signal) {
	if cmd != nil {
		_ = procutil.KillCommandTree(cmd)
		return
	}
	if proc != nil {
		_ = proc.Kill()
	}
}
