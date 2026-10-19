package cmd

import (
	"context"
	"fmt"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/app/ask"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

type askFinalization struct {
	ctx               context.Context
	cmd               *cobra.Command
	collector         *ask.TextCollector
	outputTool        *tools.SetOutputTool
	outputRequest     func() core.Request
	progressive       bool
	progressiveResult progressiveRunResult
	provider          core.Provider
	engine            *engine.Engine
	store             session.Store
	session           *session.Session
	agent             *agents.Agent
	json              bool
	jsonEmit          *ask.JSONEmitter
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
		if err := ask.EnsureOutputToolCaptured(f.ctx, f.provider, f.engine.Tools(), f.outputRequest(), f.outputTool, text); err != nil {
			if f.store != nil && f.session != nil {
				_ = f.store.UpdateStatus(f.ctx, f.session.ID, session.StatusError)
			}
			if f.json {
				_ = ask.EmitFatalError(f.jsonEmit, f.stats, err)
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
		if err := ask.EmitFinal(f.jsonEmit, f.stats, f.jsonTotalTokens); err != nil {
			return fmt.Errorf("emit final: %w", err)
		}
		f.jsonFinalPending = false
	}
	f.compactionUsages.merge(f.stats)
	if f.showStats && f.stats != nil && !f.json {
		f.stats.Finalize()
		setEstimatedStatsCost(f.stats, activeModel(f.config))
		fmt.Fprintln(f.cmd.ErrOrStderr(), f.stats.Render())
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
				_ = ask.EmitFatalError(f.jsonEmit, f.stats, err)
				f.jsonFinalPending = false
			}
			return err
		}
		output := f.outputText()
		if output == "" {
			return nil
		}
		if f.json {
			if err := ask.EmitOnCompleteStarted(f.jsonEmit); err != nil {
				return err
			}
			result, err := ask.RunOnCompleteCapture(f.agent.OnComplete, output)
			if result.Stdout != "" {
				if emitErr := ask.EmitOnCompleteOutput(f.jsonEmit, result.Stdout); emitErr != nil {
					return emitErr
				}
			}
			if err != nil {
				if emitErr := ask.EmitOnCompleteFailed(f.jsonEmit, result.Stderr, err); emitErr != nil {
					return emitErr
				}
				fmt.Fprintf(f.cmd.ErrOrStderr(), "warning: on_complete failed: %v\n", err)
			} else if err := ask.EmitOnCompleteCompleted(f.jsonEmit, result.Stderr); err != nil {
				return err
			}
			return nil
		}
		if err := ask.RunOnComplete(f.agent.OnComplete, output); err != nil {
			fmt.Fprintf(f.cmd.ErrOrStderr(), "warning: on_complete failed: %v\n", err)
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
			if err := ask.WriteCommitEditMsg(output); err != nil {
				fmt.Fprintf(f.cmd.ErrOrStderr(), "warning: failed to write commit message: %v\n", err)
			} else {
				fmt.Fprintln(f.cmd.ErrOrStderr(), "\nCommit message written to .git/COMMIT_EDITMSG and .git/GITGUI_MSG")
				fmt.Fprintln(f.cmd.ErrOrStderr(), "Run 'git commit' to use it.")
			}
		}
	}
	return nil
}
