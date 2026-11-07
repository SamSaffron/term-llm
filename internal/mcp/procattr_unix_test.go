//go:build !windows

package mcp

import "syscall"

func detachedProcess(attr *syscall.SysProcAttr) bool {
	return attr != nil && attr.Setsid
}
