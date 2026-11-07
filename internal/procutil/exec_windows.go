//go:build windows

package procutil

import (
	"os/exec"
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
// window and prepares an owned Job Object for its process tree.
//
// Callers MUST start the command through StartCommand, RunCommand, or
// OutputCommand. Those helpers create the child suspended, assign it to the job,
// and resume it synchronously. Direct exec.Cmd.Start/Run bypasses ownership.
// If a configured command will not be started, call KillCommandTree (or the
// cleanup returned by PrepareCommand) to release its job handle and registry
// entry.
func ConfigureDetachedCommand(cmd *exec.Cmd) {
	ensureSysProcAttr(cmd)
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup | createNoWindow
	configureProcessGroupCancellation(cmd)
	registerCommandTree(cmd)
}

// ConfigureCommandProcessGroup starts cmd in its own process group so it can
// receive its own console control events while keeping the caller's console.
// It also prepares an owned Job Object; see ConfigureDetachedCommand.
func ConfigureCommandProcessGroup(cmd *exec.Cmd) {
	ensureSysProcAttr(cmd)
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
	configureProcessGroupCancellation(cmd)
	registerCommandTree(cmd)
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

// KillCommandTree terminates every process in cmd's owned Job Object and
// releases the job handle. It is safe to call after exec.Cmd.Wait, before
// Wait, concurrently with Wait, and more than once. PID values are never used
// for termination, so a reused PID cannot be targeted.
func KillCommandTree(cmd *exec.Cmd) error {
	return terminateCommandTree(cmd)
}

// StartCommand starts a command prepared by ConfigureDetachedCommand or
// ConfigureCommandProcessGroup, assigns its suspended process to the owned Job
// Object, and resumes it. On any setup failure the process is terminated before
// the error is returned.
func StartCommand(cmd *exec.Cmd) error {
	return startCommandTree(cmd)
}

// RunCommand is exec.Cmd.Run for commands prepared by ConfigureDetachedCommand
// or ConfigureCommandProcessGroup.
func RunCommand(cmd *exec.Cmd) error {
	if err := StartCommand(cmd); err != nil {
		return err
	}
	return cmd.Wait()
}

// OutputCommand is exec.Cmd.Output for commands prepared by
// ConfigureDetachedCommand or ConfigureCommandProcessGroup.
func OutputCommand(cmd *exec.Cmd) ([]byte, error) {
	return outputCommandTree(cmd)
}

// releaseCommand closes the owned Job Object for a prepared command.
func releaseCommand(cmd *exec.Cmd) { _ = KillCommandTree(cmd) }
