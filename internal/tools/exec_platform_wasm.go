//go:build js && wasm

package tools

import "os/exec"

// WebAssembly has no processes to signal and no ETXTBSY exec race.

func platformKillProcessGroup(cmd *exec.Cmd) {}

func platformKillPid(pid int) {}

func isTextFileBusy(error) bool { return false }
