package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/session/scopedb"
)

// ScopeRun is the host-resolved identity of one runtime turn.
type ScopeRun struct {
	Agent, ProviderKey string
	// Model is the runtime's current model, used when the request names none.
	Model string
	// Workspace is the runtime's effective tool directory ("" when unbound).
	Workspace string
	Policy    modelpolicy.Policy
	Config    *config.Config
	// Detached marks a stateless run that neither loads nor persists the
	// session row: its history is caller-supplied. An unlocked detached run
	// tolerates an unreadable store; every other run fails closed.
	Detached bool
	// Tools is the runtime's engine registry. A locked run whose own agent
	// does not enable search loses the always-registered search tools.
	Tools interface{ UnregisterTool(string) }
}

// AdmitSessionScope runs before history reaches a provider. A locked runtime
// persists (or narrows) the durable lock atomically and marks the returned
// context so host tools receive trusted session context. An unlocked runtime
// may not serve a locked row. Ordinary unlocked sessions pay one read. On
// error the input context and policy are returned unchanged. A detached
// unlocked run (stateless, not persisted) proceeds when the row is unreadable.
func AdmitSessionScope(ctx context.Context, store session.Store, req *core.Request, run ScopeRun) (context.Context, modelpolicy.Policy, error) {
	if !run.Policy.LockSession {
		if store == nil || req.SessionID == "" {
			return ctx, run.Policy, nil
		}
		// A missing row cannot be locked. Any other lookup failure (busy,
		// undecodable policy) fails closed: the row might be locked.
		saved, err := store.Get(ctx, req.SessionID)
		if errors.Is(err, session.ErrNotFound) || (err == nil && saved == nil) {
			return ctx, run.Policy, nil
		}
		if err != nil {
			if run.Detached {
				return ctx, run.Policy, nil
			}
			return ctx, run.Policy, fmt.Errorf("check session scope: %w", err)
		}
		if saved.ModelPolicy.LockSession {
			return ctx, run.Policy, fmt.Errorf("%w: locked session requires its locked agent runtime", scopedb.ErrScope)
		}
		return ctx, run.Policy, nil
	}
	if store == nil || req.SessionID == "" {
		return ctx, run.Policy, fmt.Errorf("%w: locked agent %q requires durable session storage", scopedb.ErrScope, run.Agent)
	}
	admitter, ok := scopeAdmitter(store)
	if !ok {
		return ctx, run.Policy, fmt.Errorf("%w: session store cannot admit locked sessions atomically", scopedb.ErrScope)
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(run.Model)
	}
	// The current model and any continuation are checked against the merged
	// durable policy inside the admission transaction, before anything is
	// persisted or sent.
	effective, err := admitter.AdmitSessionScope(ctx, scopedb.Admission{SessionID: req.SessionID, Agent: run.Agent, Workspace: run.Workspace, Policy: run.Policy,
		Check: func(effective modelpolicy.Policy) error {
			if err := effective.CheckWithConfig(run.Config, run.Agent, run.ProviderKey, model); err != nil {
				return err
			}
			return checkLockedContinuation(effective, *req, run)
		}})
	if err != nil {
		return ctx, run.Policy, err
	}
	enforceLockedAgentSearch(req, run)
	// Provider payload debug logging would copy the transcript outside the
	// session store; locked runs never emit it.
	req.Debug, req.DebugRaw = false, false
	return core.ContextWithLockedSession(ctx, req.SessionID), effective, nil
}

// enforceLockedAgentSearch applies the locked agent's own search setting: a
// request, UI toggle or host --search flag cannot enable web search, and the
// engine's always-registered web_search/read_url become neither offered nor
// callable. An unloadable definition fails closed.
func enforceLockedAgentSearch(req *core.Request, run ScopeRun) {
	if run.Config != nil {
		if agent, err := LoadPersistedAgent(run.Agent, run.Config); err == nil && agent != nil && agent.Search {
			return
		}
	}
	req.Search = false
	kept := req.Tools[:0:0]
	for _, spec := range req.Tools {
		if spec.Name != core.WebSearchToolName && spec.Name != core.ReadURLToolName {
			kept = append(kept, spec)
		}
	}
	req.Tools = kept
	if run.Tools != nil {
		run.Tools.UnregisterTool(core.WebSearchToolName)
		run.Tools.UnregisterTool(core.ReadURLToolName)
	}
}

// checkLockedContinuation closes an engine checkpoint's inner request: it must
// belong to the same session and its saved model must satisfy the lock.
func checkLockedContinuation(policy modelpolicy.Policy, req core.Request, run ScopeRun) error {
	if req.Resume == nil {
		return nil
	}
	saved := req.Resume.Request
	if saved.SessionID != "" && saved.SessionID != req.SessionID {
		return fmt.Errorf("%w: locked continuation cannot change session", scopedb.ErrScope)
	}
	if model := strings.TrimSpace(saved.Model); model != "" {
		return policy.CheckWithConfig(run.Config, run.Agent, run.ProviderKey, model)
	}
	return nil
}

// scopeAdmitter unwraps logging decorators to find native scope admission.
func scopeAdmitter(store session.Store) (scopedb.Admitter, bool) {
	for store != nil {
		if admitter, ok := store.(scopedb.Admitter); ok {
			return admitter, true
		}
		logging, ok := store.(*session.LoggingStore)
		if !ok || logging == nil {
			return nil, false
		}
		store = logging.Store
	}
	return nil, false
}

// RefuseLockedProgressive fails closed for progressive runs, which bypass
// per-turn scope admission.
func RefuseLockedProgressive(policy modelpolicy.Policy, progressive bool) error {
	if progressive && policy.LockSession {
		return fmt.Errorf("%w: locked sessions do not support progressive runs", scopedb.ErrScope)
	}
	return nil
}

// RefuseLockedAux fails closed for host operations that send a session's
// history to a provider outside the per-turn scope admission (side questions,
// cross-model swap handovers, manual compaction). Locked conversations do not
// support them; ordinary sessions are unaffected.
func RefuseLockedAux(policy modelpolicy.Policy, operation string) error {
	return RefuseLockedAuxIf(policy.LockSession, operation)
}

// RefuseLockedAuxIf is RefuseLockedAux for a lock known from several sources,
// such as a stored row and a live runtime whose row is not yet locked.
func RefuseLockedAuxIf(locked bool, operation string) error {
	if locked {
		return fmt.Errorf("%w: %s are unavailable in locked sessions", scopedb.ErrScope, operation)
	}
	return nil
}
