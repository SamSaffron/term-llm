// Package scopedb owns durable conversation scope locks: the immutable-identity
// trigger and atomic admission of locked session policies.
package scopedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/modelpolicy"
	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

// LockedSQL is true for a locked sessions row. Generic full-row writers keep
// a locked row's agent and policy instead of applying (possibly stale) values.
const LockedSQL = "COALESCE(json_extract(model_policy, '$.lock_session'), 0) = 1"

// FrozenWorkspaceSQL is true once a locked sessions row has history; generic
// writers then keep its cwd and worktree.
const FrozenWorkspaceSQL = "(" + LockedSQL + " AND EXISTS (SELECT 1 FROM messages WHERE session_id = sessions.id))"

// ExcludeLockedClause returns a sessions-alias ("s.") filter omitting locked
// rows when exclude is set, for listings that feed other models or memory.
func ExcludeLockedClause(exclude bool) string {
	if !exclude {
		return ""
	}
	return " AND COALESCE(json_extract(s.model_policy, '$.lock_session'), 0) = 0"
}

// SchemaV70 freezes a locked session's identity inside SQLite so every writer
// fails closed. The lock cannot be cleared and the persisted rules must remain
// an unchanged prefix of the new rules (rules only narrow); the agent is fixed;
// the workspace (cwd, worktree and an assigned project) is fixed once the
// transcript has messages. A first project assignment of an unchanged
// workspace remains allowed.
const SchemaV70 = `
CREATE TRIGGER IF NOT EXISTS sessions_scope_lock_bu BEFORE UPDATE ON sessions
WHEN COALESCE(json_extract(OLD.model_policy, '$.lock_session'), 0) = 1
BEGIN
 SELECT RAISE(ABORT, 'locked session scope is immutable')
 WHERE COALESCE(json_extract(NEW.model_policy, '$.lock_session'), 0) <> 1
  OR EXISTS (SELECT 1 FROM json_each(OLD.model_policy, '$.rules') AS old_rule
             WHERE old_rule.value IS NOT json_extract(NEW.model_policy, '$.rules[' || old_rule.key || ']'))
  OR COALESCE(NEW.agent, '') <> COALESCE(OLD.agent, '')
  OR ((COALESCE(NEW.cwd, '') <> COALESCE(OLD.cwd, '')
       OR COALESCE(NEW.worktree_dir, '') <> COALESCE(OLD.worktree_dir, '')
       OR (COALESCE(OLD.project_id, '') <> '' AND COALESCE(NEW.project_id, '') <> OLD.project_id))
      AND EXISTS (SELECT 1 FROM messages WHERE session_id = OLD.id));
END;
`

// MigrateV70 installs the scope-lock trigger. Existing rows carry no lock.
func MigrateV70(db sqliteutil.Executor) error {
	if _, err := db.Exec(SchemaV70); err != nil {
		return fmt.Errorf("create session scope lock trigger: %w", err)
	}
	return nil
}

// Admitter is implemented by stores that can atomically admit locked
// conversations. Hosts must refuse a locked run when a store lacks it.
type Admitter interface {
	AdmitSessionScope(ctx context.Context, in Admission) (modelpolicy.Policy, error)
}

// PolicyJSON encodes a session policy column value; nil means no policy.
func PolicyJSON(policy modelpolicy.Policy) any {
	if !policy.Restricted() && !policy.LockSession {
		return nil
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return nil
	}
	return string(encoded)
}

// ErrScope reports a refused locked-session admission.
var ErrScope = errors.New("session scope lock")

// Admission is the host-resolved identity of one run.
type Admission struct {
	SessionID string
	Agent     string
	// Workspace is the runtime's effective directory; empty means unbound.
	Workspace string
	// Policy is the runtime's effective policy. It must be locked
	// (modelpolicy.Lock) when the runtime's agent opts into lock_session.
	Policy modelpolicy.Policy
	// Check validates the run against the merged effective policy before
	// anything is persisted; an error aborts admission.
	Check func(modelpolicy.Policy) error
}

func scopeErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrScope}, args...)...)
}

// Admit validates the run against the latest durable row under one SQLite
// writer reservation and persists a new or narrowed lock. It returns the
// committed effective policy. Unlocked rows with unlocked runtimes are not
// modified.
func Admit(ctx context.Context, db *sql.DB, in Admission, available bool) (modelpolicy.Policy, error) {
	if !available {
		return modelpolicy.Policy{}, scopeErr("session store lacks model policy support")
	}
	effective := in.Policy
	err := sqliteutil.WithImmediateMigrationTx(ctx, db, func(tx sqliteutil.Executor) error {
		var raw sql.NullString
		var agent, cwd, worktree string
		var hasMessages bool
		err := tx.QueryRow(`SELECT model_policy, COALESCE(agent, ''), COALESCE(cwd, ''), COALESCE(worktree_dir, ''),
			EXISTS (SELECT 1 FROM messages WHERE session_id = sessions.id) FROM sessions WHERE id = ?`, in.SessionID).
			Scan(&raw, &agent, &cwd, &worktree, &hasMessages)
		if errors.Is(err, sql.ErrNoRows) {
			if in.Policy.LockSession {
				return scopeErr("locked session %q is not persisted", in.SessionID)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("load session scope: %w", err)
		}
		var saved modelpolicy.Policy
		if raw.Valid && strings.TrimSpace(raw.String) != "" {
			if err := json.Unmarshal([]byte(raw.String), &saved); err != nil {
				return fmt.Errorf("decode session policy: %w", err)
			}
		}
		switch {
		case !saved.LockSession && !in.Policy.LockSession:
			return nil
		case saved.LockSession && !in.Policy.LockSession:
			return scopeErr("locked session requires its locked agent runtime")
		case !saved.LockSession && hasMessages:
			return scopeErr("cannot lock a conversation that already has history; start a new conversation")
		}
		if agent != "" && agent != in.Agent {
			return scopeErr("locked session cannot change agent from %q to %q", agent, in.Agent)
		}
		if saved.LockSession && hasMessages {
			if savedWorkspace := firstNonEmpty(worktree, cwd); savedWorkspace != "" && in.Workspace != "" && filepath.Clean(savedWorkspace) != filepath.Clean(in.Workspace) {
				return scopeErr("locked session cannot change workspace")
			}
		}
		effective = saved.Append(in.Policy)
		effective.LockSession = true
		if in.Check != nil {
			if err := in.Check(effective); err != nil {
				return err
			}
		}
		// An unbound row without history adopts the runtime's workspace in
		// this same transaction, before any tool runs; the trigger freezes it
		// once the first message lands.
		bind := !hasMessages && firstNonEmpty(worktree, cwd) == "" && strings.TrimSpace(in.Workspace) != ""
		if bind {
			cwd = filepath.Clean(in.Workspace)
		}
		if saved.LockSession && agent != "" && !bind && len(effective.Rules) == len(saved.Rules) {
			return nil
		}
		encoded, err := json.Marshal(effective)
		if err != nil {
			return fmt.Errorf("encode session policy: %w", err)
		}
		if _, err := tx.Exec(`UPDATE sessions SET model_policy = ?, agent = ?, cwd = ? WHERE id = ?`, string(encoded), in.Agent, nullIfEmpty(cwd), in.SessionID); err != nil {
			return fmt.Errorf("persist session scope lock: %w", err)
		}
		return nil
	})
	if err != nil {
		return modelpolicy.Policy{}, err
	}
	return effective, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
