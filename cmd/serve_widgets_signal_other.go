//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package cmd

import (
	"context"

	"github.com/samsaffron/term-llm/internal/widgets"
)

func installWidgetStopSignal(context.Context, *widgets.Manager) func() {
	return func() {}
}
