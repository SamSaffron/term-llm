package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

type askExecutionResult struct {
	stats            *ui.SessionStats
	progressive      progressiveRunResult
	jsonTotalTokens  int
	jsonFinalPending bool
	skipFinalization bool
}

type askStreamingExecution struct {
	ctx             context.Context
	cmd             *cobra.Command
	cfg             *config.Config
	toolMgr         *tools.ToolManager
	store           session.Store
	session         *session.Session
	events          <-chan ui.StreamEvent
	adapter         *ui.StreamAdapter
	runner          run.Runner
	request         run.Request
	applyResult     func(run.Result)
	persistence     *askAssistantPersistence
	collector       *textCollector
	turnStart       time.Time
	jsonEmit        *jsonEmitter
	jsonInfo        sessionInfo
	useRichRenderer bool
	teaProgram      *tea.Program
}

func (e *askStreamingExecution) run() (askExecutionResult, error) {
	result := askExecutionResult{stats: e.adapter.Stats()}
	events := e.events
	if e.useRichRenderer && e.toolMgr != nil {
		_, e.teaProgram = newAskRendererProgram(e.cfg, e.toolMgr, e.store, e.session, events)
	}
	render := func(ctx context.Context, events <-chan ui.StreamEvent) error {
		if e.teaProgram != nil {
			return runAskStreamProgram(ctx, e.teaProgram)
		}
		return streamWithRenderer(ctx, e.cfg, events, e.store, e.session)
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
		timeout := askRunnerCleanupTimeout
		if timeout <= 0 {
			timeout = run.DefaultRunnerCleanupTimeout
		}
		if !run.WaitForRunnerDone(context.Background(), runnerDone, timeout) {
			err := streamCtx.Err()
			if err == nil {
				err = context.Canceled
			}
			results <- runnerStreamResult{err: err, detached: true}
			fmt.Fprintf(e.cmd.ErrOrStderr(), "warning: runner did not stop within %s after stream cancellation; detaching\n", timeout)
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
	case askJSON:
		if err := emitSessionStarted(e.jsonEmit, e.jsonInfo); err != nil {
			cancelStream()
			wait()
			return result, err
		}
		result.jsonTotalTokens, jsonStreamErr, displayErr = streamJSONEvents(streamCtx, events, e.jsonEmit)
		result.jsonFinalPending = true
	case e.useRichRenderer:
		displayErr = render(streamCtx, events)
	default:
		displayErr = streamPlainText(streamCtx, events, askPorcelain, e.cmd.ErrOrStderr())
	}
	tools.ClearAskUserHooks()
	finishInterrupted := func() (askExecutionResult, error) {
		if e.persistence != nil && e.collector != nil {
			dbCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
			_ = e.persistence.persistInterrupted(dbCtx, e.collector.Text(), time.Since(e.turnStart).Milliseconds())
			cancel()
		}
		if e.store != nil && e.session != nil {
			_ = e.store.UpdateStatus(context.Background(), e.session.ID, session.StatusInterrupted)
			_ = e.store.SetCurrent(context.Background(), e.session.ID)
		}
		if askJSON && result.jsonFinalPending {
			if err := emitFinal(e.jsonEmit, result.stats, result.jsonTotalTokens); err != nil {
				return result, fmt.Errorf("emit final: %w", err)
			}
			result.jsonFinalPending = false
		}
		result.skipFinalization = true
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
		if askJSON && !isTerminalFlushed(displayErr) {
			_ = emitFatalError(e.jsonEmit, result.stats, displayErr)
			result.jsonFinalPending = false
		}
		return result, displayErr
	}
	if askJSON && jsonStreamErr != nil {
		cancelStream()
		if !streamDone {
			streamErr = wait().err
			streamDone = true
		}
		if interrupted(jsonStreamErr) {
			return finishInterrupted()
		}
		if err := emitFinal(e.jsonEmit, result.stats, result.jsonTotalTokens); err != nil {
			return result, fmt.Errorf("emit final: %w", err)
		}
		result.jsonFinalPending = false
		return result, &terminalFlushedError{err: jsonStreamErr}
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
		if askJSON {
			_ = emitFatalError(e.jsonEmit, result.stats, err)
			result.jsonFinalPending = false
		}
		return result, err
	}
	return result, nil
}
