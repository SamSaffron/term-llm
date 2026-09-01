//go:build (aix || android || darwin || dragonfly || freebsd || illumos || ios || netbsd || openbsd || solaris) && !linux

package cmd

import "syscall"

// SIGHUP plus process-group signaling is the portable fallback. Linux also
// enumerates the owning login session so job-control groups cannot survive.
func signalServeShellSession(_ int, _ syscall.Signal) error { return nil }
