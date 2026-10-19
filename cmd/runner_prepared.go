package cmd

import (
	"context"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

// RunnerOptions captures explicit inputs to the shared command runner.
type RunnerOptions = cmdRunnerOptions

// Runner is the shared prepared-run implementation.
type Runner = cmdRunner

// PreparedRun owns the runtime and resources prepared for one invocation.
type PreparedRun = cmdRunEnvironment

// RunResult is the runtime's result, including pending client-tool calls.
type RunResult = serveRunResult

// NewRunner constructs a runner with a concrete preparation API.
func NewRunner(cfg *config.Config, opts RunnerOptions) *Runner {
	return newCmdRunner(cfg, opts).(*cmdRunner)
}

// NewCmdRunner constructs the same runner behind the neutral run.Runner interface.
func NewCmdRunner(cfg *config.Config, opts RunnerOptions) runpkg.Runner {
	return newCmdRunner(cfg, opts)
}

// Prepare resolves inputs and owns resources until PreparedRun.Close.
func (r *cmdRunner) Prepare(ctx context.Context, req runpkg.Request, sink runpkg.EventSink) (*PreparedRun, error) {
	return r.prepare(ctx, req, sink)
}

// CheckAgentModel validates admission without exposing the private resolution tuple.
func (r *cmdRunner) CheckAgentModel(ctx context.Context, req runpkg.Request) error {
	_, _, _, _, err := r.resolveRunModel(ctx, req)
	return err
}

// Request returns the prepared provider request by value.
func (p *cmdRunEnvironment) Request() core.Request { return p.llmReq }

// Engine returns the prepared engine without transferring its ownership.
func (p *cmdRunEnvironment) Engine() *engine.Engine { return p.engine }

// ConfigureInteractiveSink installs the sink's interactive tool callbacks.
func (p *cmdRunEnvironment) ConfigureInteractiveSink(sink runpkg.EventSink) {
	configureInteractiveSink(p.runtime.toolMgr, sink)
}

// RunWithEvents executes through the prepared runtime's event path.
func (p *cmdRunEnvironment) RunWithEvents(ctx context.Context, stateful, replaceHistory bool, msgs []core.Message, req core.Request, onEvent func(core.Event) error) (RunResult, error) {
	return p.runtime.RunWithEvents(ctx, stateful, replaceHistory, msgs, req, onEvent)
}
