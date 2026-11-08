//go:build !windows

package testutil

import "github.com/samsaffron/term-llm/internal/testutil/processinfo"

// ProcessHasExited reports whether pid no longer has executable code.
func ProcessHasExited(pid int) bool { return processinfo.ProcessHasExited(pid) }

func procStatState(data []byte) (byte, bool) { return processinfo.ProcStatState(data) }
