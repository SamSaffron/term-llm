//go:build windows

package clibin

import "syscall"

const createNewProcessGroup = 0x00000200

func detachedProcess(attr *syscall.SysProcAttr) bool {
	return attr != nil && attr.CreationFlags&createNewProcessGroup != 0
}
