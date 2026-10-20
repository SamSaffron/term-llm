package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	appchat "github.com/samsaffron/term-llm/internal/app/chat"
	"github.com/samsaffron/term-llm/internal/termhost"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

func (r *chatInvocation) run(ctx context.Context, args []string) error {
	interactive := r.ownsTerminalHost()
	if len(r.opts.AutoSend) == 0 && !interactive {
		return fmt.Errorf("chat requires an interactive terminal; use --auto-send for non-interactive execution")
	}

	// Extract @agent from args if present, and get remaining args as initial text
	atAgent, filteredArgs := ExtractAgentFromArgs(args)
	cliAgent := strings.TrimSpace(r.opts.Agent)
	if atAgent != "" && cliAgent == "" {
		cliAgent = atAgent
	}
	initialText := strings.Join(filteredArgs, " ")

	resumeRequested := r.opts.Changed.Resume
	resumeID := strings.TrimSpace(r.opts.Resume)

	handoverAutoSend := ""
	relaunchHandoff := chatRelaunchHandoff{}
	var restored chat.ReloadState
	if ok, err := r.deps.RestoreReloadState("chat", &restored); err != nil {
		return err
	} else if ok {
		initialText = ""
		r.opts.AutoSend = nil
		resumeRequested = restored.Meta == nil
		resumeID = restored.SessionID
		if restored.Provider != "" && restored.Model != "" {
			r.opts.Provider = restored.Provider + ":" + restored.Model
		}
		relaunchHandoff.reloadState = &restored
	}
	mainRuns := chat.NewMainRunManager(ctx)
	defer mainRuns.Close(5 * time.Second)
	launchConfig, err := r.deps.LoadConfig()
	if err != nil {
		return err
	}
	var lifecycleManager *termhost.Manager
	var lifecycleReporter appchat.LifecycleReporter
	if interactive {
		lifecycleManager, err = termhost.New(launchConfig.Lifecycle, legacyTerminalProgressEnabled(launchConfig))
		if err != nil {
			return fmt.Errorf("initialize terminal-host lifecycle: %w", err)
		}
		defer lifecycleManager.Close()
		defer appchat.RestoreLifecycleOSC(lifecycleManager)
		lifecycleReporter = lifecycleManager
	}
	r.handoverApproval = nil
	for {
		nextResumeID, nextAutoSend, err := r.runChatOnce(ctx, initialText, cliAgent, resumeRequested, resumeID, handoverAutoSend, &relaunchHandoff, mainRuns, lifecycleReporter, launchConfig)
		launchConfig = nil // Later relaunches intentionally reload configuration.
		if err != nil {
			return err
		}
		if nextResumeID == "" {
			return nil
		}

		// Relaunch with full session runtime state restored.
		resumeRequested = true
		resumeID = nextResumeID
		initialText = ""
		handoverAutoSend = nextAutoSend
	}
}
