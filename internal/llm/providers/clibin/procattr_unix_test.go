//go:build !windows

package clibin

import "syscall"

func detachedProcess(attr *syscall.SysProcAttr) bool {
	return attr != nil && attr.Setsid
}
