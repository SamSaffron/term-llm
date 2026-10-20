package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	runapproval "github.com/samsaffron/term-llm/internal/app/runtime/approval"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/exitcode"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

func (r *chatInvocation) runChatOnce(ctx context.Context, initialText, cliAgent string, resumeRequested bool, resumeID, handoverAutoSend string, relaunchHandoff *chatRelaunchHandoff, mainRuns *chat.MainRunManager, lifecycleReporter LifecycleReporter, launchConfig *config.Config) (string, string, error) {
	rt, err := r.buildChatSessionRuntime(ctx, chatSessionLaunch{
		initialText:      initialText,
		cliAgent:         cliAgent,
		resumeRequested:  resumeRequested,
		resumeID:         resumeID,
		handoverAutoSend: handoverAutoSend,
		relaunchHandoff:  relaunchHandoff,
		config:           launchConfig,
	}, mainRuns)
	if err != nil {
		return "", "", err
	}
	if r.rendererOwnsTerminal() {
		closeLog, logErr := r.deps.StartDiagnostics(r.deps.Stderr)
		if logErr != nil {
			rt.cleanupResources()
			return "", "", logErr
		}
		defer closeLog()
	}

	// In-process session switches replace the active runtime while the program
	// keeps running, so all teardown paths resolve the runtime late.
	var runtimeMu sync.Mutex
	activeRT := rt
	var activeUnwire func()
	programDone := false
	currentRuntime := func() *chatSessionRuntime {
		runtimeMu.Lock()
		defer runtimeMu.Unlock()
		return activeRT
	}

	defer func() {
		runtimeMu.Lock()
		programDone = true
		cur := activeRT
		runtimeMu.Unlock()
		// Transfer runtime ownership when this session still has an active
		// background run; the manager invokes cleanup only after provider
		// execution, persistence callbacks, and subscriber delivery stopped.
		if mainRuns != nil && mainRuns.AdoptResources(cur.model.SessionID(), cur.cleanupResources) {
			return
		}
		// Engine shutdown is bounded. Runtime-owned operations get the same
		// cancellation budget, but resources are deliberately left open if one
		// ignores cancellation; closing SQLite underneath it is never safe.
		if cur.model.CancelAndWaitStreamDone() {
			cur.cleanupResources()
		}
	}()
	defer func() { currentRuntime().restoreTitle() }()
	defer func() {
		runtimeMu.Lock()
		unwire := activeUnwire
		runtimeMu.Unlock()
		if unwire != nil {
			unwire()
		}
	}()

	// Build program options. AltScreen and mouse mode are declarative on the View in v2.
	programInput, err := r.buildChatProgramInput(len(r.opts.AutoSend) > 0)
	if err != nil {
		return "", "", err
	}
	defer programInput.cleanup()

	var opts []tea.ProgramOption
	opts = append(opts, tea.WithoutSignalHandler())
	if programInput.disableInput {
		opts = append(opts, tea.WithInput(nil))
	} else if programInput.reader != nil {
		opts = append(opts, tea.WithInput(programInput.reader))
	}

	// Run the TUI. Image bytes and their acknowledgements are attached directly
	// to the tea.View that composed them; stdout remains renderer-owned. The host
	// model keeps the Program stable across session-model replacements and drops
	// delayed command results from models that are no longer visible.
	reloadEnabled := r.ownsTerminalHost()
	if reloadEnabled {
		rt.model.EnableProcessReload()
	}
	if relaunchHandoff != nil && relaunchHandoff.reloadState != nil {
		rt.model.RestoreReloadState(*relaunchHandoff.reloadState)
		relaunchHandoff.reloadState = nil
	}
	programModel := NewProgramModel(rt.model, lifecycleReporter)
	if reloadEnabled {
		programModel.EnableReloadScope()
	}
	p := tea.NewProgram(programModel, opts...)
	rt.model.SetProgram(p)

	// In-process session switching: /fork, /thread, /tree branches, and
	// /resume selections swap the visible model inside this running program
	// instead of quitting and relaunching, so the terminal never flashes out
	// of the alternate screen. /handover and /reload keep quit-and-relaunch.
	var switcher chat.SessionSwitcher
	switcher = func(request chat.SessionSwitchRequest) (*chat.Model, error) {
		next, buildErr := r.buildChatSessionRuntime(ctx, chatSessionLaunch{
			cliAgent:        cliAgent,
			resumeRequested: true,
			resumeID:        request.SessionID,
			warningSink:     currentRuntime().storeWarnings,
			relaunchHandoff: &chatRelaunchHandoff{
				branchPrefill:   request.BranchPrefill,
				branchPathNotes: request.BranchPathNotes,
				branchAutoSend:  request.BranchAutoSend,
			},
		}, mainRuns)
		if buildErr != nil {
			return nil, buildErr
		}
		next.model.SetProgram(p)
		next.model.SetSessionSwitcher(switcher)
		if reloadEnabled {
			next.model.EnableProcessReload()
		}

		runtimeMu.Lock()
		if programDone {
			runtimeMu.Unlock()
			next.cleanupResources()
			return nil, errors.New("chat program already exited")
		}
		prev := activeRT
		prevUnwire := activeUnwire
		activeRT = next
		activeUnwire = nil
		runtimeMu.Unlock()

		// Release process-global UI hooks before installing the replacements.
		// Runtime-owned callbacks stay bound to prev so its adopted background
		// run keeps routing prompts to the owning session.
		if prevUnwire != nil {
			prevUnwire()
		}
		unwire := wireChatSessionUI(ctx, next, p, mainRuns, false)
		runtimeMu.Lock()
		activeUnwire = unwire
		runtimeMu.Unlock()

		disposeChatSessionRuntime(prev, mainRuns)
		return next.model, nil
	}
	rt.model.SetSessionSwitcher(switcher)

	runtimeMu.Lock()
	activeUnwire = wireChatSessionUI(ctx, rt, p, mainRuns, true)
	runtimeMu.Unlock()

	// Wire OS signal handling to kill the Bubble Tea program and restore the
	// terminal. Ctrl+C in raw TUI mode is handled as a keypress by the model;
	// this path covers real SIGINT/SIGTERM (for example from another terminal).
	go func() {
		<-ctx.Done()
		killed := make(chan struct{})
		go func() {
			p.Kill()
			close(killed)
		}()
		select {
		case <-killed:
		case <-time.After(2 * time.Second):
			fmt.Fprintln(os.Stderr, "term-llm: forced exit after interrupt")
			// Bubble Tea failed to stop, so restore OSC directly through /dev/tty
			// before spending the remaining bounded budget releasing adapters.
			RestoreLifecycleOSCToTTY(lifecycleReporter)
			os.Exit(130)
		}
	}()

	stopReload := func() {}
	if reloadEnabled {
		stopReload = BindReload(ctx, p, programModel, lifecycleReporter, r.deps.ExecReload)
	}
	defer stopReload()
	programFinalModel, runErr := p.Run()
	stopReload()
	currentRuntime().restoreTitle()
	RestoreLifecycleOSC(lifecycleReporter)

	if runErr != nil {
		if ctx.Err() != nil && errors.Is(runErr, tea.ErrProgramKilled) {
			return "", "", exitcode.Cancel()
		}
		return "", "", fmt.Errorf("failed to run chat: %w", runErr)
	}
	finalModel, err := FinalModel(programFinalModel)
	if err != nil {
		return "", "", fmt.Errorf("extract final chat model: %w", err)
	}

	cur := currentRuntime()
	nextResumeID := finalModel.RequestedResumeSessionID()
	nextHandoverAutoSend := finalModel.RequestedHandoverAutoSend()
	if relaunchHandoff != nil {
		relaunchHandoff.branchPrefill = finalModel.RequestedBranchPrefill()
		relaunchHandoff.branchPathNotes = finalModel.RequestedBranchPathNotes()
		relaunchHandoff.branchAutoSend = finalModel.RequestedBranchAutoSend()
	}
	// Carry a user-selected mode only into an actual handover. Ordinary /resume
	// and /new relaunches must resolve the target session/config independently.
	mode := finalModel.ApprovalModeRequested()
	r.handoverApproval = runapproval.ChatCarryForRelaunch(finalModel.ApprovalModeChanged(), nextHandoverAutoSend, mode)
	r.opts.Approval = mode.String()
	r.opts.Yolo = mode == tools.ModeYolo
	r.opts.AutoApproval = mode == tools.ModeAuto

	// Handle /reload: stop every runtime-owned resource, then re-exec under the
	// potentially new binary. Do not release lifecycle claims before Exec: the
	// successor immediately republishes them without an idle gap. If Exec fails,
	// this function returns and runChat's bounded deferred close releases them.
	if finalModel.WantsReload() {
		cur.cleanupResources()
		sessionID := finalModel.ReloadSessionID()
		if execErr := r.deps.ExecReload(sessionID); execErr != nil {
			// exec failed (shouldn't happen on Unix) — fall through and exit normally
			fmt.Fprintf(r.deps.Stderr, "reload: %v\n", execErr)
		}
		return "", "", nil
	}

	// The model creates fresh sessions; cur.sess is only the launch-time resume
	// input and remains nil for a new chat. Read the final model's identity.
	// Print only after the renderer has released the terminal.
	if nextResumeID == "" {
		printChatResumeHint(r.deps.Stdout, cur.store, finalModel.SessionID())
	}

	return nextResumeID, nextHandoverAutoSend, nil
}
