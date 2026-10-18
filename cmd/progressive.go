package cmd

import (
	"context"

	"github.com/samsaffron/term-llm/internal/app/runtime/progressive"
	"github.com/samsaffron/term-llm/internal/llm"
)

// Compatibility aliases keep cmd callers stable until runner, ask and serve move.
type progressiveStopWhen = progressive.StopWhen

const (
	progressiveStopWhenDone    = progressive.StopWhenDone
	progressiveStopWhenTimeout = progressive.StopWhenTimeout
)

type askProgressiveOptions = progressive.AskOptions
type progressiveRunOptions = progressive.RunOptions
type progressiveRunResult = progressive.RunResult
type progressTracker = progressive.Tracker
type progressCommit = progressive.Commit

func newProgressTracker() *progressTracker { return progressive.NewTracker() }
func isProgressToolName(name string) bool  { return progressive.IsProgressTool(name) }
func validateAskProgressiveOptions(opts *askProgressiveOptions) error {
	return progressive.ValidateAskOptions(opts)
}
func runProgressiveSession(ctx context.Context, engine *llm.Engine, req llm.Request, opts progressiveRunOptions) (progressiveRunResult, error) {
	return progressive.RunSession(ctx, engine, req, opts)
}
func buildProgressiveRunResult(id, reason string, finalized bool, commit *progressCommit, fallback string) progressiveRunResult {
	return progressive.BuildRunResult(id, reason, finalized, commit, fallback)
}
func progressiveSyntheticUserMessageContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return progressive.SyntheticUserMessageContext(ctx)
}
