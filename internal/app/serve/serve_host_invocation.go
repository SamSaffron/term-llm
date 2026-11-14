package serve

import (
	"net"

	"github.com/term-llm/term-llm/internal/config"
	runpkg "github.com/term-llm/term-llm/internal/run"
)

type serveDeps struct {
	Invocation
	Listener        net.Listener
	PlatformFactory servePlatformFactory
	PlatformDelay   servePlatformDelay
	PlatformReady   func()
	RunnerFactory   func(*config.Config, cmdRunnerOptions) runpkg.Runner
}
