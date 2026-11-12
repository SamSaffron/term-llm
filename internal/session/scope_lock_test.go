package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	"github.com/samsaffron/term-llm/internal/session/scopedb"
)

func newScopeTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func lockedTestPolicy() modelpolicy.Policy {
	return modelpolicy.Lock(nil, modelpolicy.Policy{}.With("private", []string{"debug:allowed"}))
}

func createScopeSession(t *testing.T, store *SQLiteStore, agent, cwd string) *Session {
	t.Helper()
	sess := &Session{ID: NewID(), Provider: "debug", Model: "allowed", Agent: agent, CWD: cwd}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestAdmitSessionScopeLocksAndFreezesIdentity(t *testing.T) {
	ctx := context.Background()
	store := newScopeTestStore(t)
	sess := createScopeSession(t, store, "private", "/work/a")
	effective, err := store.AdmitSessionScope(ctx, scopedb.Admission{SessionID: sess.ID, Agent: "private", Policy: lockedTestPolicy()})
	if err != nil || !effective.LockSession {
		t.Fatalf("admit = %+v, %v", effective, err)
	}
	addBranchTestMessage(t, store, sess.ID, core.UserText("private text"))
	got, err := store.Get(ctx, sess.ID)
	if err != nil || !got.ModelPolicy.LockSession || len(got.ModelPolicy.Rules) != 1 {
		t.Fatalf("persisted = %+v, %v", got, err)
	}

	// Generic full-row writers with stale identity keep the locked scope but
	// still persist allowed metadata (status, title, usage).
	stale := *got
	stale.Agent, stale.CWD, stale.WorktreeDir = "other", "/work/b", "/work/b"
	stale.ModelPolicy = modelpolicy.Policy{}.With("other", []string{"other:*"})
	stale.Name, stale.Status = "renamed", StatusComplete
	if err := store.Update(ctx, &stale); err != nil {
		t.Fatalf("stale metadata update failed: %v", err)
	}
	after, _ := store.Get(ctx, sess.ID)
	if !after.ModelPolicy.LockSession || len(after.ModelPolicy.Rules) != 1 || after.ModelPolicy.Rules[0].Agent != "private" ||
		after.Agent != "private" || after.CWD != "/work/a" || after.WorktreeDir != "" || after.Name != "renamed" || after.Status != StatusComplete {
		t.Fatalf("after stale update = %+v", after)
	}
	// Raw writers fail closed in SQLite itself.
	looser := `{"rules":[{"agent":"private","allowed":["debug:*","other:*"]}],"lock_session":true}`
	for name, stmt := range map[string]string{
		"clear policy":       `UPDATE sessions SET model_policy = NULL WHERE id = ?`,
		"unlock":             `UPDATE sessions SET model_policy = '{"rules":[{"agent":"private","allowed":["debug:allowed"]}]}' WHERE id = ?`,
		"same-length loosen": `UPDATE sessions SET model_policy = '` + looser + `' WHERE id = ?`,
		"longer, changed prefix": `UPDATE sessions SET model_policy = '{"rules":[{"agent":"private","allowed":["other:*"]},` +
			`{"agent":"private","allowed":["debug:allowed"]}],"lock_session":true}' WHERE id = ?`,
		"retarget agent":      `UPDATE sessions SET agent = 'other' WHERE id = ?`,
		"move workspace":      `UPDATE sessions SET cwd = '/work/b' WHERE id = ?`,
		"bind other worktree": `UPDATE sessions SET worktree_dir = '/work/b' WHERE id = ?`,
	} {
		if _, err := store.db.Exec(stmt, sess.ID); err == nil {
			t.Fatalf("raw SQL %s succeeded on a locked row", name)
		}
	}

	// Admission rejects a different agent and an unlocked runtime.
	if _, err := store.AdmitSessionScope(ctx, scopedb.Admission{SessionID: sess.ID, Agent: "other", Policy: lockedTestPolicy()}); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("other agent admit = %v", err)
	}
	if _, err := store.AdmitSessionScope(ctx, scopedb.Admission{SessionID: sess.ID, Agent: "private"}); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("unlocked runtime admit = %v", err)
	}
	// Narrowing persists; repeats are idempotent.
	narrower := lockedTestPolicy().With("child", []string{"debug:allowed"})
	if effective, err = store.AdmitSessionScope(ctx, scopedb.Admission{SessionID: sess.ID, Agent: "private", Policy: narrower}); err != nil || len(effective.Rules) != 2 {
		t.Fatalf("narrow admit = %+v, %v", effective, err)
	}
	if after, _ = store.Get(ctx, sess.ID); len(after.ModelPolicy.Rules) != 2 {
		t.Fatalf("narrowing not persisted: %+v", after.ModelPolicy)
	}
}

func TestAdmitSessionScopeBindsUnboundWorkspaceBeforeHistory(t *testing.T) {
	ctx := context.Background()
	store := newScopeTestStore(t)
	sess := createScopeSession(t, store, "private", "")
	if _, err := store.AdmitSessionScope(ctx, scopedb.Admission{SessionID: sess.ID, Agent: "private", Workspace: "/work/a", Policy: lockedTestPolicy()}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, sess.ID); got.CWD != "/work/a" {
		t.Fatalf("unbound locked row cwd = %q, want runtime workspace", got.CWD)
	}
	addBranchTestMessage(t, store, sess.ID, core.UserText("private text"))
	if _, err := store.AdmitSessionScope(ctx, scopedb.Admission{SessionID: sess.ID, Agent: "private", Workspace: "/work/b", Policy: lockedTestPolicy()}); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("retargeted workspace admit = %v", err)
	}
}

func TestAdmitSessionScopeRefusesRetrofitAndKeepsUnlockedRowsUnchanged(t *testing.T) {
	ctx := context.Background()
	store := newScopeTestStore(t)
	plain := createScopeSession(t, store, "other", "")
	addBranchTestMessage(t, store, plain.ID, core.UserText("ordinary"))
	if _, err := store.AdmitSessionScope(ctx, scopedb.Admission{SessionID: plain.ID, Agent: "other"}); err != nil {
		t.Fatalf("unlocked admit = %v", err)
	}
	if _, err := store.AdmitSessionScope(ctx, scopedb.Admission{SessionID: plain.ID, Agent: "other", Policy: lockedTestPolicy()}); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("retrofit admit = %v, want refusal", err)
	}
	got, _ := store.Get(ctx, plain.ID)
	if got.ModelPolicy.LockSession || got.ModelPolicy.Restricted() {
		t.Fatalf("unlocked row changed: %+v", got.ModelPolicy)
	}
	// Ordinary sessions keep full metadata mobility.
	got.Agent, got.CWD = "another", "/elsewhere"
	if err := store.Update(ctx, got); err != nil {
		t.Fatalf("ordinary update: %v", err)
	}
	if _, err := store.AdmitSessionScope(ctx, scopedb.Admission{SessionID: "missing", Agent: "private", Policy: lockedTestPolicy()}); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("missing locked row admit = %v", err)
	}
}

func TestScopeLockedSessionsExcludedFromPrivacyListings(t *testing.T) {
	ctx := context.Background()
	store := newScopeTestStore(t)
	locked := createScopeSession(t, store, "private", "")
	plain := createScopeSession(t, store, "other", "")
	if _, err := store.AdmitSessionScope(ctx, scopedb.Admission{SessionID: locked.ID, Agent: "private", Policy: lockedTestPolicy()}); err != nil {
		t.Fatal(err)
	}
	all, err := store.List(ctx, ListOptions{Limit: -1})
	if err != nil || len(all) != 2 {
		t.Fatalf("List = %d, %v", len(all), err)
	}
	filtered, err := store.List(ctx, ListOptions{Limit: -1, ExcludeScopeLocked: true})
	if err != nil || len(filtered) != 1 || filtered[0].ID != plain.ID {
		t.Fatalf("filtered List = %+v, %v", filtered, err)
	}
}

func TestBranchOfLockedSessionKeepsLock(t *testing.T) {
	ctx := context.Background()
	store := newScopeTestStore(t)
	sess := createScopeSession(t, store, "private", "/work/a")
	if _, err := store.AdmitSessionScope(ctx, scopedb.Admission{SessionID: sess.ID, Agent: "private", Policy: lockedTestPolicy()}); err != nil {
		t.Fatal(err)
	}
	anchor := addBranchTestMessage(t, store, sess.ID, core.UserText("branch me"))
	branch, err := store.CreateBranch(ctx, sess.ID, CreateBranchOptions{AnchorMessageID: anchor.ID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, branch.Session.ID)
	if err != nil || !got.ModelPolicy.LockSession || got.Agent != "private" || got.CWD != "/work/a" {
		t.Fatalf("branch = %+v, %v", got, err)
	}
}
