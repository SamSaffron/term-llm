//go:build !windows

package process

import (
	"os"
	"syscall"
)

func ownedState(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}
