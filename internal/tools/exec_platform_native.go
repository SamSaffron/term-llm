//go:build !js && !windows

package tools

import (
	"errors"
	"syscall"
)

func isTextFileBusy(err error) bool { return errors.Is(err, syscall.ETXTBSY) }
