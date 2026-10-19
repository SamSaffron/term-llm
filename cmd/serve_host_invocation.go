package cmd

import (
	"net"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

type serveDeps struct {
	appserve.Invocation
	Listener        net.Listener
	PlatformFactory servePlatformFactory
	PlatformDelay   servePlatformDelay
	PlatformReady   func()
	RunnerFactory   func(*config.Config, cmdRunnerOptions) runpkg.Runner
}
