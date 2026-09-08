//go:build windows

package process

import "os"

func ownedState(os.FileInfo) bool { return false }
