package runtime

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/session/scopedb"
)

// CheckLockedSessionScope refuses retargeting a locked conversation before any
// runtime, history projection or provider is built. Empty requested values
// reuse the durable identity; they never select a new one.
func CheckLockedSessionScope(sess *session.Session, agentName, cwd string) error {
	if sess == nil || !sess.ModelPolicy.LockSession {
		return nil
	}
	if agentName = strings.TrimSpace(agentName); agentName != "" && agentName != sess.Agent {
		return fmt.Errorf("%w: locked session cannot change agent from %q to %q", scopedb.ErrScope, sess.Agent, agentName)
	}
	saved := strings.TrimSpace(sess.WorktreeDir)
	if saved == "" {
		saved = strings.TrimSpace(sess.CWD)
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" && saved != "" && filepath.Clean(cwd) != filepath.Clean(saved) {
		return fmt.Errorf("%w: locked session cannot change workspace", scopedb.ErrScope)
	}
	return nil
}

// RefuseLockedDirectRun fails closed for surfaces that run their own engine
// outside the serve runner's per-turn scope admission (direct `ask`, the
// interactive chat TUI). Locked conversations run through `term-llm serve`.
func RefuseLockedDirectRun(sess *session.Session, agent *agents.Agent, surface string) error {
	if (agent != nil && agent.LockSession) || (sess != nil && sess.ModelPolicy.LockSession) {
		return fmt.Errorf("%w: locked sessions are unavailable in %s; use term-llm serve (web/API or jobs)", scopedb.ErrScope, surface)
	}
	return nil
}

// SessionModelPolicy returns a durable session's effective policy: its
// persisted chain (including any lock) plus its own agent's current rule.
// An agent whose definition is missing keeps only the persisted chain; an
// invalid definition fails rather than lifting the agent's restriction.
func SessionModelPolicy(cfg *config.Config, sess *session.Session) (modelpolicy.Policy, error) {
	if sess == nil {
		return modelpolicy.Policy{}, fmt.Errorf("session is unavailable")
	}
	if strings.TrimSpace(sess.Agent) == "" {
		return sess.ModelPolicy, nil
	}
	if cfg == nil {
		return modelpolicy.Policy{}, fmt.Errorf("load session agent %q: configuration is unavailable", sess.Agent)
	}
	agent, err := LoadPersistedAgent(sess.Agent, cfg)
	if err != nil {
		return modelpolicy.Policy{}, fmt.Errorf("load session agent %q for model policy: %w", sess.Agent, err)
	}
	return ModelPolicyForSession(sess, agent), nil
}

// PersistedRunPolicy is the policy stored with a run's session: the inherited
// chain, or for a locked run the complete chain including its own rule.
func PersistedRunPolicy(inherited, effective modelpolicy.Policy) modelpolicy.Policy {
	if effective.LockSession {
		return effective
	}
	return inherited
}
