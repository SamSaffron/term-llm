package ask

import (
	"context"
	"fmt"
	"io"

	"github.com/samsaffron/term-llm/internal/agents"
	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	"github.com/samsaffron/term-llm/internal/app/runtime/progressive"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

type askFinalization struct {
	ctx               context.Context
	stderr            io.Writer
	collector         *TextCollector
	outputTool        *tools.SetOutputTool
	outputRequest     func() core.Request
	progressive       bool
	progressiveResult progressive.RunResult
	provider          core.Provider
	engine            *engine.Engine
	store             session.Store
	session           *session.Session
	agent             *agents.Agent
	json              bool
	jsonEmit          *JSONEmitter
	stats             *ui.SessionStats
	jsonTotalTokens   int
	jsonFinalPending  bool
	compactionUsages  *compactionUsageCollector
	showStats         bool
	config            *config.Config
}

// finalizeAsk performs required-output validation, durable terminal state,
// completion hooks, terminal JSON emission, and final statistics in that order.
func (f *askFinalization) run() error {
	if f.collector != nil {
		f.collector.Wait()
	}
	if f.outputTool != nil {
		text := ""
		if f.collector != nil {
			text = f.collector.Text()
		} else if f.progressive {
			text = progressiveOutputText(f.progressiveResult)
		}
		if err := EnsureOutputToolCaptured(f.ctx, f.provider, f.engine.Tools(), f.outputRequest(), f.outputTool, text); err != nil {
			if f.store != nil && f.session != nil {
				_ = f.store.UpdateStatus(f.ctx, f.session.ID, session.StatusError)
			}
			if f.json {
				_ = EmitFatalError(f.jsonEmit, f.stats, err)
				f.jsonFinalPending = false
			}
			return err
		}
	}
	if f.store != nil && f.session != nil {
		_ = f.store.UpdateStatus(f.ctx, f.session.ID, session.StatusComplete)
		_ = f.store.SetCurrent(f.ctx, f.session.ID)
	}
	if err := f.runCompletionHook(); err != nil {
		return err
	}
	if f.json && f.jsonFinalPending {
		if err := EmitFinal(f.jsonEmit, f.stats, f.jsonTotalTokens); err != nil {
			return fmt.Errorf("emit final: %w", err)
		}
		f.jsonFinalPending = false
	}
	f.compactionUsages.merge(f.stats)
	if f.showStats && f.stats != nil && !f.json {
		f.stats.Finalize()
		appruntime.SetEstimatedStatsCost(f.stats, bootstrap.ActiveModel(f.config))
		fmt.Fprintln(f.stderr, f.stats.Render())
	}
	return nil
}
func (f *askFinalization) outputText() string {
	if f.outputTool != nil {
		return f.outputTool.Value()
	}
	if f.collector != nil {
		return f.collector.Text()
	}
	if f.progressive {
		return progressiveOutputText(f.progressiveResult)
	}
	return ""
}
func (f *askFinalization) runCompletionHook() error {
	if f.agent != nil && f.agent.OnComplete != "" {
		if f.outputTool != nil && !f.outputTool.Captured() {
			err := fmt.Errorf("output tool %q did not produce a value", f.outputTool.Name())
			if f.json {
				_ = EmitFatalError(f.jsonEmit, f.stats, err)
				f.jsonFinalPending = false
			}
			return err
		}
		output := f.outputText()
		if output == "" {
			return nil
		}
		if f.json {
			if err := EmitOnCompleteStarted(f.jsonEmit); err != nil {
				return err
			}
			result, err := RunOnCompleteCapture(f.agent.OnComplete, output)
			if result.Stdout != "" {
				if emitErr := EmitOnCompleteOutput(f.jsonEmit, result.Stdout); emitErr != nil {
					return emitErr
				}
			}
			if err != nil {
				if emitErr := EmitOnCompleteFailed(f.jsonEmit, result.Stderr, err); emitErr != nil {
					return emitErr
				}
				fmt.Fprintf(f.stderr, "warning: on_complete failed: %v\n", err)
			} else if err := EmitOnCompleteCompleted(f.jsonEmit, result.Stderr); err != nil {
				return err
			}
			return nil
		}
		if err := RunOnComplete(f.agent.OnComplete, output); err != nil {
			fmt.Fprintf(f.stderr, "warning: on_complete failed: %v\n", err)
		}
		return nil
	}
	if f.agent != nil && f.agent.Output == "commit_editmsg" {
		output := ""
		if f.collector != nil {
			output = f.collector.Text()
		} else if f.progressive {
			output = progressiveOutputText(f.progressiveResult)
		}
		if output != "" {
			if err := WriteCommitEditMsg(output); err != nil {
				fmt.Fprintf(f.stderr, "warning: failed to write commit message: %v\n", err)
			} else {
				fmt.Fprintln(f.stderr, "\nCommit message written to .git/COMMIT_EDITMSG and .git/GITGUI_MSG")
				fmt.Fprintln(f.stderr, "Run 'git commit' to use it.")
			}
		}
	}
	return nil
}
