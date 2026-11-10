//go:build !windows && !js

package codemode

import "syscall"

const snapshotNonblock = syscall.O_NONBLOCK
