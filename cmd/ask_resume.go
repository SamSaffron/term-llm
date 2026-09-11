package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
)

func finalizeAskSessionSettings(settings *SessionSettings, sess *session.Session, selected *sessionInputSelection, setup *skills.Setup, cfg *config.Config, provider llm.Provider, resuming bool) (string, string) {
	sessionID := ""
	if sess != nil {
		sessionID = sess.ID
	}
	sessionID = ensureRequestSessionID(sessionID, resuming)
	settings.SessionID = sessionID
	baseInputPrompt := settings.SystemPrompt
	settings.SystemPrompt = InjectSkillsMetadata(settings.SystemPrompt, setup)
	if selected != nil {
		settings.SystemPrompt = selected.Prompt
	}
	alignSettingsToActiveProvider(settings, cfg, provider)
	return sessionID, baseInputPrompt
}

func prepareAskResume(ctx context.Context, cmd *cobra.Command, cfg *config.Config, agent *agents.Agent, store session.Store, settings *SessionSettings) (*session.Session, *sessionInputTicket, *sessionInputSelection, bool, error) {
	resuming := cmd.Flags().Changed("resume")
	if !resuming {
		return nil, nil, nil, false, nil
	}
	if store == nil {
		return nil, nil, nil, true, fmt.Errorf("session storage is disabled; cannot resume")
	}
	var sess *session.Session
	resumeID := strings.TrimSpace(askResume)
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

	_ = store.SetCurrent(ctx, sess.ID)
	_ = store.UpdateStatus(ctx, sess.ID, session.StatusActive)
	if _, supported := session.AsSessionInputRefresher(store); !supported {
		var err error
		settings.SystemPrompt, settings.Tools, err = resolveSessionPromptTools(cfg, agent, CLIFlags{Tools: askTools, ToolsSet: cmd.Flags().Changed("tools"), SystemMessage: askSystemMessage, SystemMessageSet: cmd.Flags().Changed("system"), Files: askFiles, Platform: "console"}, cfg.Ask.Instructions, settings.BaseDir, settings.Provider, settings.Model)
		if err != nil {
			return nil, nil, nil, true, err
		}
	}
	if !cmd.Flags().Changed("search") {
		settings.Search = sess.Search
	}
	if !cmd.Flags().Changed("tools") {
		settings.Tools = sess.Tools
	}
	if !cmd.Flags().Changed("mcp") {
		settings.MCP = sess.MCP
	}
	if agent == nil && strings.TrimSpace(sess.Agent) != "" {
		if resumedAgent, err := LoadAgent(sess.Agent, cfg); err == nil && resumedAgent != nil {
			settings.PlanGuidance = resumedAgent.Name == "developer" && resumedAgent.Source == agents.SourceBuiltin
		}
	}
	if err := RestoreWorktreeBinding(ctx, store, sess, nil); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to restore session directory: %v\n", err)
	}
	if dir := effectiveSessionDirectory(sess); dir != "" {
		if canonical, err := canonicalRuntimeDir(dir); err == nil {
			settings.BaseDir = canonical
			settings.ShellWorkingDir = canonical
			settings.PrimaryWorkspace = canonical
		}
	}

	var ticket *sessionInputTicket
	var selected *sessionInputSelection
	if _, supported := session.AsSessionInputRefresher(store); supported {
		var err error
		ticket, err = processSessionInputs.acquire(ctx, store, sess.ID, inputBinding(sess.Agent, settings.BaseDir))
		if err != nil {
			return nil, nil, nil, true, err
		}
		selected = ticket.selected()
		if selected != nil {
			settings.SystemPrompt, settings.Tools = selected.BasePrompt, selected.Tools
		} else {
			resumedAgent, err := LoadAgent(sess.Agent, cfg)
			if err != nil {
				ticket.fail()
				return nil, ticket, nil, true, err
			}
			settings.SystemPrompt, settings.Tools, err = resolveSessionPromptTools(cfg, resumedAgent, CLIFlags{
				Provider: askProvider, Tools: askTools, ToolsSet: cmd.Flags().Changed("tools"),
				SystemMessage: askSystemMessage, SystemMessageSet: cmd.Flags().Changed("system"), Files: askFiles, Platform: "console",
			}, cfg.Ask.Instructions, settings.BaseDir, cfg.Ask.Provider, cfg.Ask.Model)
			if err != nil {
				ticket.fail()
				return nil, ticket, nil, true, err
			}
		}
	}
	return sess, ticket, selected, true, nil
}
