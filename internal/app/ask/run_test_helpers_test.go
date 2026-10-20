package ask

import (
	"io"

	"github.com/samsaffron/term-llm/internal/app/runtime/approval"
	"github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	"github.com/samsaffron/term-llm/internal/config"
)

func testDependencies(stdout, stderr io.Writer) Dependencies {
	return Dependencies{
		Stdout: stdout, Stderr: stderr,
		Interactive:       func() bool { return false },
		LoadConfig:        bootstrap.LoadConfigWithSetup,
		CreateDebugLogger: bootstrap.CreateDebugLogger,
		ResolveApproval: func(cfg *config.Config) (approval.ResolvedMode, error) {
			return approval.Resolve(approval.ResolutionInput{Surface: approval.SurfaceAsk, Config: cfg})
		},
	}
}
