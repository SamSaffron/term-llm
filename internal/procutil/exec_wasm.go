//go:build js && wasm

package procutil

import "os/exec"

// WebAssembly hosts have no process groups or sessions; subprocesses are
// unavailable there, so these are no-ops kept for API compatibility.

// ConfigureDetachedCommand is a no-op on WebAssembly.
func ConfigureDetachedCommand(cmd *exec.Cmd) {}

// ConfigureCommandProcessGroup is a no-op on WebAssembly.
func ConfigureCommandProcessGroup(cmd *exec.Cmd) {}

// StartCommand is exec.Cmd.Start on WebAssembly.
func StartCommand(cmd *exec.Cmd) error { return cmd.Start() }

// RunCommand is exec.Cmd.Run on WebAssembly.
func RunCommand(cmd *exec.Cmd) error { return cmd.Run() }

// OutputCommand is exec.Cmd.Output on WebAssembly.
func OutputCommand(cmd *exec.Cmd) ([]byte, error) { return cmd.Output() }

// KillCommandTree is a no-op on WebAssembly, which has no process handles.
func KillCommandTree(*exec.Cmd) error { return nil }

// releaseCommand releases platform process-tree resources.
func releaseCommand(*exec.Cmd) {}
