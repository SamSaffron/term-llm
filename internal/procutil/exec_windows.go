//go:build windows

package procutil

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

const (
	// createNewProcessGroup gives the child its own console process group.
	createNewProcessGroup = 0x00000200
	// createNoWindow starts a console application without a visible window.
	// Unlike DETACHED_PROCESS it keeps standard handles usable, so captured
	// stdout/stderr continue to work for detached helpers.
	createNoWindow = 0x08000000
)

// ConfigureDetachedCommand starts cmd in a new process group with no console
// window and configures process-tree cancellation.
func ConfigureDetachedCommand(cmd *exec.Cmd) {
	ensureSysProcAttr(cmd)
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup | createNoWindow
	configureProcessGroupCancellation(cmd)
}

// ConfigureCommandProcessGroup starts cmd in its own process group so it can
// receive its own console control events while keeping the caller's console.
func ConfigureCommandProcessGroup(cmd *exec.Cmd) {
	ensureSysProcAttr(cmd)
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
	configureProcessGroupCancellation(cmd)
}

func ensureSysProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
}

func configureProcessGroupCancellation(cmd *exec.Cmd) {
	// exec.CommandContext installs a default Cancel hook; exec.Command leaves it
	// nil and os/exec rejects a non-nil Cancel on such commands at Start. Only
	// replace the hook when context cancellation is actually in play.
	if cmd.Cancel == nil {
		return
	}
	cmd.Cancel = func() error { return KillCommandTree(cmd) }
}

// KillCommandTree terminates cmd and its descendants. Windows has no process
// group kill, so taskkill /T walks the process tree. The os.Process handle is
// still open at this point, which keeps the PID reserved until Wait/Release:
// the taskkill lookup therefore cannot target an unrelated process that reused
// the PID. If taskkill is unavailable, fall back to terminating the direct
// child only.
func KillCommandTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := kill.Run(); err == nil {
		return nil
	}
	if cmd.ProcessState != nil {
		return nil
	}
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, os.ErrInvalid) {
		return nil
	}
	return err
}
