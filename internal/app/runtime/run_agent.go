package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

// ResolveRunAgent resolves one request and its persisted provenance before model
// selection. Fresh requests retain registry trust; resumes use conservative
// legacy-reference resolution and preserve the session's inherited model rules.
func ResolveRunAgent(ctx context.Context, req runpkg.Request, cfg *config.Config, store session.Store) (*agents.Agent, modelpolicy.Policy, runpkg.ParentModel, error) {
	inherited, resume := req.ModelPolicy, runpkg.ParentModel{}
	var sess *session.Session
	if req.SessionID != "" && store != nil {
		var err error
		sess, err = store.Get(ctx, req.SessionID)
		if err != nil {
			return nil, inherited, resume, fmt.Errorf("load resumed session policy: %w", err)
		}
	}
	var agent *agents.Agent
	var err error
	if sess != nil {
		inherited = sess.ModelPolicy.Append(inherited)
		if req.Resume {
			resume = runpkg.ParentModel{Provider: ResolveSessionProviderKey(cfg, sess), Model: sess.Model}
		}
	}
	persisted := sess != nil && ((req.AgentName != "" && req.AgentName == sess.Agent) || (req.Resume && strings.TrimSpace(req.AgentName) == ""))
	if persisted {
		agent, err = LoadPersistedAgent(sess.Agent, cfg)
	} else {
		agent, err = LoadAgent(req.AgentName, cfg)
	}
	if err != nil {
		return nil, inherited, resume, err
	}
	if !persisted && strings.TrimSpace(req.AgentName) != "" && agent == nil {
		return nil, inherited, resume, fmt.Errorf("agent %q not found", req.AgentName)
	}
	return agent, inherited, resume, nil
}
