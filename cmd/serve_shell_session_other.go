//go:build (aix || android || dragonfly || freebsd || illumos || ios || netbsd || openbsd || solaris) && !linux

package cmd

import "syscall"

// SIGHUP plus process-group signaling is the portable fallback on platforms
// without process-tree enumeration for job-control groups.
func signalServeShellSession(_ int, _ syscall.Signal) error { return nil }
