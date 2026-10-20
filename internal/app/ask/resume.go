package ask

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
)

func finalizeAskSessionSettings(settings *appserve.SessionSettings, sess *session.Session, selected *appserve.SessionInputSelection, setup *skills.Setup, cfg *config.Config, provider core.Provider, resuming bool) (string, string) {
	sessionID := ""
	if sess != nil {
		sessionID = sess.ID
	}
	sessionID = appserve.EnsureRequestSessionID(sessionID, resuming)
	settings.SessionID = sessionID
	baseInputPrompt := settings.SystemPrompt
	settings.SystemPrompt = appserve.InjectSkillsMetadata(settings.SystemPrompt, setup)
	if selected != nil {
		settings.SystemPrompt = selected.Prompt
	}
	appserve.AlignSettingsToActiveProvider(settings, cfg, provider)
	return sessionID, baseInputPrompt
}

func prepareAskResume(ctx context.Context, opts Options, stderr io.Writer, cfg *config.Config, agent *agents.Agent, store session.Store, settings *appserve.SessionSettings) (*session.Session, *appserve.SessionInputTicket, *appserve.SessionInputSelection, bool, error) {
	resuming := opts.Changed.Resume
	if !resuming {
		return nil, nil, nil, false, nil
	}
	if store == nil {
		return nil, nil, nil, true, fmt.Errorf("session storage is disabled; cannot resume")
	}
	var sess *session.Session
	resumeID := strings.TrimSpace(opts.Resume)
	if resumeID == "" {
		sess, _ = store.GetCurrent(ctx)
		if sess == nil {
			summaries, _ := store.List(ctx, session.ListOptions{Limit: 1})
			if len(summaries) > 0 {
				sess, _ = store.Get(ctx, summaries[0].ID)
			}
		}
	} else {
		sess, _ = store.GetByPrefix(ctx, resumeID)
	}
	if sess == nil {
		return nil, nil, nil, true, fmt.Errorf("no session to resume")
	}
	// Structured attachments imply transient tools even when the raw persisted
	// tool list is empty. Derive these before SetupToolManager; file existence is
	// intentionally not required here because exact grants are restored later.
	deriveAskResumeAutoTools(ctx, store, sess, settings)

	// ask applied its provider overrides to cfg before resuming; build the
	// prompt for that provider/model rather than the agent's preference.
	promptProvider, promptModel := appruntime.ActiveLLMFlags(cfg)
	_ = store.SetCurrent(ctx, sess.ID)
	_ = store.UpdateStatus(ctx, sess.ID, session.StatusActive)
	if _, supported := session.AsSessionInputRefresher(store); !supported {
		var err error
		settings.SystemPrompt, settings.Tools, err = appserve.ResolveSessionPromptTools(cfg, agent, appserve.CLIFlags{ActiveProvider: promptProvider, ActiveModel: promptModel, Tools: opts.Tools, ToolsSet: opts.Changed.Tools, SystemMessage: opts.SystemMessage, SystemMessageSet: opts.Changed.System, Files: opts.Files, Platform: "console", MemoryPaths: settings.MemoryPaths}, cfg.Ask.Instructions, settings.BaseDir, settings.Provider, settings.Model)
		if err != nil {
			return nil, nil, nil, true, err
		}
	}
	settings.Search = appruntime.ResumedSessionSearch(settings.Search, sess.Search, opts.Changed.Search, opts.NoSearch)
	if !opts.Changed.Tools {
		settings.Tools = sess.Tools
	}
	if !opts.Changed.MCP {
		settings.MCP = sess.MCP
	}
	if agent == nil && strings.TrimSpace(sess.Agent) != "" {
		if resumedAgent, err := appruntime.LoadAgent(sess.Agent, cfg); err == nil && resumedAgent != nil {
			settings.PlanGuidance = resumedAgent.Name == "developer" && resumedAgent.Source == agents.SourceBuiltin
			settings.Workspace = resumedAgent.Workspace
		}
	}
	if err := appserve.RestoreWorktreeBinding(ctx, store, sess, nil); err != nil {
		fmt.Fprintf(stderr, "warning: failed to restore session directory: %v\n", err)
	}
	if dir := appruntime.EffectiveSessionDirectory(sess); dir != "" {
		if canonical, err := appserve.CanonicalRuntimeDir(dir); err == nil {
			settings.BaseDir = canonical
			settings.ShellWorkingDir = canonical
			settings.PrimaryWorkspace = canonical
		}
	}

	var ticket *appserve.SessionInputTicket
	var selected *appserve.SessionInputSelection
	if _, supported := session.AsSessionInputRefresher(store); supported {
		var err error
		ticket, err = appserve.ProcessSessionInputs().Acquire(ctx, store, sess.ID, appserve.InputBinding(sess.Agent, settings.BaseDir))
		if err != nil {
			return nil, nil, nil, true, err
		}
		selected = ticket.Selected()
		if selected != nil {
			settings.SystemPrompt, settings.Tools = selected.BasePrompt, selected.Tools
		} else {
			resumedAgent, err := appruntime.LoadAgent(sess.Agent, cfg)
			if err != nil {
				ticket.Fail()
				return nil, ticket, nil, true, err
			}
			settings.SystemPrompt, settings.Tools, err = appserve.ResolveSessionPromptTools(cfg, resumedAgent, appserve.CLIFlags{
				Provider: opts.Provider, ActiveProvider: promptProvider, ActiveModel: promptModel, Tools: opts.Tools, ToolsSet: opts.Changed.Tools,
				SystemMessage: opts.SystemMessage, SystemMessageSet: opts.Changed.System, Files: opts.Files, Platform: "console", MemoryPaths: settings.MemoryPaths,
			}, cfg.Ask.Instructions, settings.BaseDir, cfg.Ask.Provider, cfg.Ask.Model)
			if err != nil {
				ticket.Fail()
				return nil, ticket, nil, true, err
			}
		}
	}
	return sess, ticket, selected, true, nil
}

func deriveAskResumeAutoTools(ctx context.Context, store session.Store, sess *session.Session, settings *appserve.SessionSettings) {
	if store == nil || sess == nil || settings == nil {
		return
	}
	rows, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		return
	}
	history := make([]core.Message, 0, len(rows))
	for i := range rows {
		history = append(history, rows[i].ToLLMMessage())
	}
	settings.AutoTools = AddAutoTools(settings.AutoTools, history)
}
