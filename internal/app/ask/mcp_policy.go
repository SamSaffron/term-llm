package ask

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/term-llm/term-llm/internal/agents"
	appruntime "github.com/term-llm/term-llm/internal/app/runtime"
	"github.com/term-llm/term-llm/internal/app/runtime/mcpstart"
	appserve "github.com/term-llm/term-llm/internal/app/serve"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/session"
)

func findAskResumeSession(ctx context.Context, opts Options, store session.Store) (*session.Session, error) {
	if store == nil {
		return nil, fmt.Errorf("session storage is disabled; cannot resume")
	}
	var sess *session.Session
	if id := strings.TrimSpace(opts.Resume); id != "" {
		sess, _ = store.GetByPrefix(ctx, id)
	} else {
		sess, _ = store.GetCurrent(ctx)
		if sess == nil {
			summaries, _ := store.List(ctx, session.ListOptions{Limit: 1})
			if len(summaries) > 0 {
				sess, _ = store.Get(ctx, summaries[0].ID)
			}
		}
	}
	if sess == nil {
		return nil, fmt.Errorf("no session to resume")
	}
	return sess, nil
}

func resolveAskRunAgent(ctx context.Context, opts Options, cfg *config.Config, store session.Store) (*agents.Agent, *session.Session, error) {
	if !opts.Changed.Resume {
		agent, err := appruntime.LoadAgent(opts.Agent, cfg)
		return agent, nil, err
	}
	sess, err := findAskResumeSession(ctx, opts, store)
	if err != nil {
		return nil, nil, err
	}
	agent, err := appruntime.LoadPersistedAgent(sess.Agent, cfg)
	if err != nil {
		return nil, nil, err
	}
	if opts.Agent != "" {
		selected, err := appruntime.LoadAgent(opts.Agent, cfg)
		if err != nil {
			return nil, nil, err
		}
		if agents.Reference(selected) != agents.Reference(agent) {
			return nil, nil, fmt.Errorf("cannot resume session for agent %q with --agent %q; resume without --agent or select the same agent", sess.Agent, opts.Agent)
		}
	}
	return agent, sess, nil
}

func restoreAskMCP(ctx context.Context, opts Options, agent *agents.Agent, store session.Store, sess *session.Session, settings *appserve.SessionSettings, warnings io.Writer, prepared *mcpstart.PreparedSelection, load mcpstart.ConfigLoader) (*mcpstart.PreparedSelection, error) {
	if prepared == nil {
		var err error
		prepared, err = mcpstart.PrepareSelection(agents.PolicyIdentity(agent), settings.MCP, opts.Changed.MCP, warnings, load)
		if err != nil {
			return nil, err
		}
	}
	settings.MCP = prepared.Active
	if sess != nil {
		if agent != nil {
			sess.Agent = agents.Reference(agent)
		}
		if prepared.CanPersist() {
			sess.MCP = settings.MCP
		}
		if err := store.Update(ctx, sess); err != nil {
			return nil, fmt.Errorf("save resumed agent/MCP selection: %w", err)
		}
	}
	return prepared, nil
}
