//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package cmd

import (
	"context"
	"errors"
)

func execWebProcess(string, []string, []string) error {
	return errors.New("self-exec is unsupported on this platform")
}
func installWebExecSignal(context.Context, *webExecCoordinator) func() { return func() {} }
