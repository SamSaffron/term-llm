package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

type askProgressiveExecution struct {
	ctx             context.Context
	cmd             *cobra.Command
	cfg             *config.Config
	toolMgr         *tools.ToolManager
	store           session.Store
	session         *session.Session
	runner          run.Runner
	request         run.Request
	options         askProgressiveOptions
	applyResult     func(run.Result)
	wrapEvents      func(<-chan ui.StreamEvent) <-chan ui.StreamEvent
	useRichRenderer bool
	jsonEmit        *jsonEmitter
	jsonInfo        sessionInfo
	captureOutput   func(string) error
}

func (e *askProgressiveExecution) run() (askExecutionResult, error) {
	bridge := newAskProgressiveBridge(ui.DefaultStreamBufferSize)
	if e.session != nil {
		bridge.Stats().SeedTotals(e.session.InputTokens, e.session.OutputTokens, e.session.CachedInputTokens, e.session.CacheWriteTokens, e.session.ToolCalls, e.session.LLMTurns+e.session.CompactionCount)
	}
	bridge.Stats().SetModel(activeModel(e.cfg))
	result := askExecutionResult{stats: bridge.Stats()}
	events := e.wrapEvents(bridge.Events())
	var drain <-chan struct{}
	if askPorcelain && !askJSON {
		done := make(chan struct{})
		drain = done
		go func() {
			defer close(done)
			for event := range events {
				if event.Type == ui.StreamEventGuardian {
					writeGuardianStatus(e.cmd.ErrOrStderr(), event.Guardian)
				}
			}
		}()
	}
	request := e.request
	request.Progressive = &run.ProgressiveOptions{Timeout: e.options.Timeout, StopWhen: string(e.options.StopWhen), ContinueWith: e.options.ContinueWith}
	runProgressive := func() askProgressiveRunResult {
		bridge.Stats().RequestStart()
		sink := askProgressiveRunnerSink{bridge: bridge, onGuardian: func(event tools.GuardianEvent) {
			if !event.Usage.BillableCountersZero() && e.store != nil && e.session != nil {
				u := event.Usage
				_ = e.store.UpdateMetrics(context.Background(), e.session.ID, 0, 0, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
			}
		}}
		runResult, err := e.runner.Run(e.ctx, request, sink)
		e.applyResult(runResult)
		progressive := progressiveRunResult{}
		if runResult.Progressive != nil {
			progressive = *progressiveFromRunResult(runResult.Progressive)
		}
		if err != nil {
			bridge.CloseError(err)
		} else {
			bridge.CloseSuccess()
		}
		return askProgressiveRunResult{Result: progressive, Err: err}
	}
	var program *tea.Program
	if e.useRichRenderer && e.toolMgr != nil {
		_, program = newAskRendererProgram(e.cfg, e.toolMgr, e.store, e.session, events)
	}
	render := func(ctx context.Context) error {
		if program != nil {
			return runAskStreamProgram(ctx, program)
		}
		return streamWithRenderer(ctx, e.cfg, events, e.store, e.session)
	}
	var progressive askProgressiveRunResult
	var jsonStreamErr, displayErr error
	if askJSON {
		if err := emitSessionStarted(e.jsonEmit, e.jsonInfo); err != nil {
			return result, err
		}
	}
	if askPorcelain && !askJSON {
		progressive = runProgressive()
		if drain != nil {
			<-drain
		}
	} else {
		runCh := make(chan askProgressiveRunResult, 1)
		go func() { runCh <- runProgressive() }()
		displayCtx := context.WithoutCancel(e.ctx)
		switch {
		case askJSON:
			var writeErr error
			result.jsonTotalTokens, jsonStreamErr, writeErr = streamJSONEvents(displayCtx, events, e.jsonEmit)
			displayErr = writeErr
		case e.useRichRenderer:
			displayErr = render(displayCtx)
		default:
			displayErr = streamPlainText(displayCtx, events, false, e.cmd.ErrOrStderr())
		}
		if displayErr != nil {
			bridge.Stop()
		}
		progressive = <-runCh
	}
	tools.ClearAskUserHooks()
	if displayErr != nil {
		if askJSON {
			_ = emitFatalError(e.jsonEmit, result.stats, displayErr)
		}
		return result, displayErr
	}
	result.progressive = progressive.Result
	if progressive.Err != nil {
		if e.store != nil && e.session != nil {
			_ = e.store.UpdateStatus(context.Background(), e.session.ID, session.StatusError)
		}
		return result, fmt.Errorf("progressive run failed: %w", progressive.Err)
	}
	if e.captureOutput != nil {
		if err := e.captureOutput(progressiveOutputText(result.progressive)); err != nil {
			if e.store != nil && e.session != nil {
				_ = e.store.UpdateStatus(context.Background(), e.session.ID, session.StatusError)
			}
			if askJSON {
				_ = emitFatalError(e.jsonEmit, result.stats, err)
			}
			return result, err
		}
	}
	status := session.StatusComplete
	switch result.progressive.ExitReason {
	case exitReasonTimeout, exitReasonCancelled:
		status = session.StatusInterrupted
	}
	if e.store != nil && e.session != nil {
		_ = e.store.UpdateStatus(context.Background(), e.session.ID, status)
		_ = e.store.SetCurrent(context.Background(), e.session.ID)
	}
	if askJSON {
		if err := emitProgressiveResult(e.jsonEmit, result.progressive); err != nil {
			return result, fmt.Errorf("emit progressive result: %w", err)
		}
		result.jsonFinalPending = true
		if jsonStreamErr != nil {
			if err := emitFinal(e.jsonEmit, result.stats, result.jsonTotalTokens); err != nil {
				return result, fmt.Errorf("emit final: %w", err)
			}
			result.jsonFinalPending = false
			return result, jsonStreamErr
		}
	} else if askPorcelain {
		if err := json.NewEncoder(e.cmd.OutOrStdout()).Encode(result.progressive); err != nil {
			return result, fmt.Errorf("encode progressive result: %w", err)
		}
	}
	return result, nil
}
