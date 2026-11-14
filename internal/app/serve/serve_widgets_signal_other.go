//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package serve

import (
	"context"

	"github.com/term-llm/term-llm/internal/widgets"
)

func installWidgetStopSignal(context.Context, *widgets.Manager) func() {
	return func() {}
}
