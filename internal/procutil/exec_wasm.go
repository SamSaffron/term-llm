//go:build js && wasm

package procutil

import "os/exec"

// WebAssembly hosts have no process groups or sessions; subprocesses are
// unavailable there, so these are no-ops kept for API compatibility.

// ConfigureDetachedCommand is a no-op on WebAssembly.
func ConfigureDetachedCommand(cmd *exec.Cmd) {}

// ConfigureCommandProcessGroup is a no-op on WebAssembly.
func ConfigureCommandProcessGroup(cmd *exec.Cmd) {}

// KillCommandTree is a no-op on WebAssembly, which has no process handles.
func KillCommandTree(*exec.Cmd) error { return nil }
