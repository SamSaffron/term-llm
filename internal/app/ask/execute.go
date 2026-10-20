package ask

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/app/runtime/progressive"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

type askExecutionResult struct {
	stats            *ui.SessionStats
	progressive      progressive.RunResult
	jsonTotalTokens  int
	jsonFinalPending bool
	skipFinalization bool
	// interrupted reports a user stop (e.g. Ctrl-C in the rich renderer) that
	// did not cancel the process context; detached children must be shut down
	// rather than drained.
	interrupted bool
}

type askStreamingExecution struct {
	ctx             context.Context
	stderr          io.Writer
	stdout          io.Writer
	flags           Options
	cleanupTimeout  time.Duration
	cfg             *config.Config
	toolMgr         *tools.ToolManager
	store           session.Store
	session         *session.Session
	events          <-chan ui.StreamEvent
	adapter         *ui.StreamAdapter
	runner          run.Runner
	request         run.Request
	applyResult     func(run.Result)
	persistence     *AssistantPersistence
	collector       *TextCollector
	turnStart       time.Time
	jsonEmit        *JSONEmitter
	jsonInfo        SessionInfo
	useRichRenderer bool
	teaProgram      *tea.Program
}

func (e *askStreamingExecution) run() (askExecutionResult, error) {
	result := askExecutionResult{stats: e.adapter.Stats()}
	events := e.events
	if e.useRichRenderer && e.toolMgr != nil {
		_, e.teaProgram = NewRendererProgram(e.cfg, e.toolMgr, e.store, e.session, events)
	}
	render := func(ctx context.Context, events <-chan ui.StreamEvent) error {
		if e.teaProgram != nil {
			return RunStreamProgram(ctx, e.teaProgram)
		}
		return StreamWithRenderer(ctx, e.cfg, events, e.store, e.session)
	}
	streamCtx, cancelStream := context.WithCancel(e.ctx)
	defer cancelStream()
	type runnerStreamResult struct {
		result   run.Result
		err      error
		detached bool
	}
	results := make(chan runnerStreamResult, 1)
	go func() {
		pipe := run.NewEventPipe(streamCtx, ui.DefaultStreamBufferSize)
		runnerResults := make(chan runnerStreamResult, 1)
		runnerDone := make(chan struct{})
		go func() {
			value, err := e.runner.Run(streamCtx, e.request, pipe)
			runnerResults <- runnerStreamResult{result: value, err: err}
			close(runnerDone)
			pipe.CloseWithError(err)
		}()
		e.adapter.ProcessStream(streamCtx, pipe)
		select {
		case <-runnerDone:
		default:
			cancelStream()
		}
		timeout := e.cleanupTimeout
		if timeout <= 0 {
			timeout = run.DefaultRunnerCleanupTimeout
		}
		if !run.WaitForRunnerDone(context.Background(), runnerDone, timeout) {
			err := streamCtx.Err()
			if err == nil {
				err = context.Canceled
			}
			results <- runnerStreamResult{err: err, detached: true}
			fmt.Fprintf(e.stderr, "warning: runner did not stop within %s after stream cancellation; detaching\n", timeout)
			return
		}
		results <- <-runnerResults
	}()
	wait := func() runnerStreamResult {
		value := <-results
		if !value.detached {
			e.applyResult(value.result)
		}
		return value
	}
	var displayErr, jsonStreamErr error
	switch {
	case e.flags.JSON:
		if err := EmitSessionStarted(e.jsonEmit, e.jsonInfo); err != nil {
			cancelStream()
			wait()
			return result, err
		}
		result.jsonTotalTokens, jsonStreamErr, displayErr = StreamJSONEvents(streamCtx, events, e.jsonEmit)
		result.jsonFinalPending = true
	case e.useRichRenderer:
		displayErr = render(streamCtx, events)
	default:
		displayErr = StreamPlainText(streamCtx, events, e.flags.Porcelain, e.stderr)
	}
	tools.ClearAskUserHooks()
	finishInterrupted := func() (askExecutionResult, error) {
		if e.persistence != nil && e.collector != nil {
			dbCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
			_ = e.persistence.PersistInterrupted(dbCtx, e.collector.Text(), time.Since(e.turnStart).Milliseconds())
			cancel()
		}
		if e.store != nil && e.session != nil {
			_ = e.store.UpdateStatus(context.Background(), e.session.ID, session.StatusInterrupted)
			_ = e.store.SetCurrent(context.Background(), e.session.ID)
		}
		if e.flags.JSON && result.jsonFinalPending {
			if err := EmitFinal(e.jsonEmit, result.stats, result.jsonTotalTokens); err != nil {
				return result, fmt.Errorf("emit final: %w", err)
			}
			result.jsonFinalPending = false
		}
		result.skipFinalization = true
		result.interrupted = true
		return result, nil
	}
	interrupted := func(err error) bool {
		return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	}
	var streamErr error
	streamDone := false
	if displayErr != nil {
		cancelStream()
		streamErr = wait().err
		streamDone = true
		if interrupted(displayErr) || interrupted(streamErr) {
			return finishInterrupted()
		}
		if e.flags.JSON && !IsTerminalFlushed(displayErr) {
			_ = EmitFatalError(e.jsonEmit, result.stats, displayErr)
			result.jsonFinalPending = false
		}
		return result, displayErr
	}
	if e.flags.JSON && jsonStreamErr != nil {
		cancelStream()
		if !streamDone {
			streamErr = wait().err
			streamDone = true
		}
		if interrupted(jsonStreamErr) {
			return finishInterrupted()
		}
		if err := EmitFinal(e.jsonEmit, result.stats, result.jsonTotalTokens); err != nil {
			return result, fmt.Errorf("emit final: %w", err)
		}
		result.jsonFinalPending = false
		return result, NewTerminalFlushedError(jsonStreamErr)
	}
	if !streamDone {
		streamErr = wait().err
	}
	if streamErr != nil {
		if e.store != nil && e.session != nil {
			status := session.StatusError
			if interrupted(streamErr) {
				status = session.StatusInterrupted
			}
			_ = e.store.UpdateStatus(context.Background(), e.session.ID, status)
		}
		if interrupted(streamErr) {
			return finishInterrupted()
		}
		err := fmt.Errorf("streaming failed: %w", streamErr)
		if e.flags.JSON {
			_ = EmitFatalError(e.jsonEmit, result.stats, err)
			result.jsonFinalPending = false
		}
		return result, err
	}
	return result, nil
}
