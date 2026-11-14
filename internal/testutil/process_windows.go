//go:build windows

package testutil

import "github.com/term-llm/term-llm/internal/testutil/processinfo"

// ProcessHasExited reports whether pid refers to a process that is no longer running.
func ProcessHasExited(pid int) bool { return processinfo.ProcessHasExited(pid) }
