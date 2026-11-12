package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/session/scopedb"
)

func TestAdmitSessionScopeMarksOnlyLockedRuns(t *testing.T) {
	ctx := context.Background()
	cfg := testSelectConfig()
	sqlite, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	store := session.NewLoggingStore(sqlite, nil) // decorators must not hide native admission
	plain := &session.Session{ID: session.NewID(), Provider: "debug", Model: "allowed", Agent: "plain"}
	locked := &session.Session{ID: session.NewID(), Provider: "debug", Model: "allowed", Agent: "private"}
	for _, s := range []*session.Session{plain, locked} {
		if err := store.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	marked := func(ctx context.Context, id string) string {
		return core.LockedSessionFromContext(core.ContextWithSessionID(ctx, id))
	}
	gotCtx, policy, err := AdmitSessionScope(ctx, store, &core.Request{SessionID: plain.ID}, ScopeRun{Agent: "plain"})
	if err != nil || marked(gotCtx, plain.ID) != "" || policy.LockSession {
		t.Fatalf("unlocked admission = %v", err)
	}
	lockedRun := ScopeRun{Agent: "private", ProviderKey: "debug", Model: "allowed", Config: cfg,
		Policy: modelpolicy.Lock(cfg, modelpolicy.Policy{}.With("private", []string{"debug:allowed"}))}
	resume := &core.Continuation{Request: core.Request{SessionID: locked.ID, Model: "denied-model"}}
	if _, _, err := AdmitSessionScope(ctx, store, &core.Request{SessionID: locked.ID, Resume: resume}, lockedRun); err == nil {
		t.Fatal("locked continuation with disallowed model admitted")
	}
	other := &core.Continuation{Request: core.Request{SessionID: "another", Model: "allowed"}}
	if _, _, err := AdmitSessionScope(ctx, store, &core.Request{SessionID: locked.ID, Resume: other}, lockedRun); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("cross-session continuation = %v", err)
	}
	gotCtx, policy, err = AdmitSessionScope(ctx, store, &core.Request{SessionID: locked.ID}, lockedRun)
	if err != nil || marked(gotCtx, locked.ID) != locked.ID || !policy.LockSession {
		t.Fatalf("locked admission = %v", err)
	}
	if _, _, err := AdmitSessionScope(ctx, store, &core.Request{SessionID: locked.ID}, ScopeRun{Agent: "private"}); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("stale unlocked runtime = %v", err)
	}
	if _, _, err := AdmitSessionScope(ctx, nil, &core.Request{SessionID: "x"}, lockedRun); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("storeless locked runtime = %v", err)
	}
	if _, _, err := AdmitSessionScope(ctx, &session.NoopStore{}, &core.Request{SessionID: "x"}, lockedRun); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("non-native locked store = %v", err)
	}
	if err := RefuseLockedProgressive(lockedRun.Policy, true); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("progressive = %v", err)
	}
	if err := RefuseLockedProgressive(modelpolicy.Policy{}, true); err != nil {
		t.Fatalf("ordinary progressive = %v", err)
	}
}

type recordingUnregister []string

func (r *recordingUnregister) UnregisterTool(name string) { *r = append(*r, name) }

func TestEnforceLockedAgentSearchFailsClosed(t *testing.T) {
	req := &core.Request{Search: true, Tools: []core.ToolSpec{{Name: core.WebSearchToolName}, {Name: "update_plan"}, {Name: core.ReadURLToolName}}}
	var removed recordingUnregister
	enforceLockedAgentSearch(req, ScopeRun{Agent: "missing-definition", Tools: &removed})
	if req.Search || len(req.Tools) != 1 || req.Tools[0].Name != "update_plan" {
		t.Fatalf("request after enforcement = %+v", req)
	}
	if len(removed) != 2 {
		t.Fatalf("unregistered = %v", removed)
	}
}

type failingGetStore struct{ *session.NoopStore }

func (failingGetStore) Get(context.Context, string) (*session.Session, error) {
	return nil, errors.New("database is locked")
}

func TestAdmitSessionScopeChecksMergedPolicyModelAndWorkspace(t *testing.T) {
	ctx := context.Background()
	cfg := testSelectConfig()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess := &session.Session{ID: session.NewID(), Provider: "debug", Model: "allowed", Agent: "private", CWD: "/work/a"}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	locked := modelpolicy.Lock(cfg, modelpolicy.Policy{}.With("private", []string{"debug:allowed"}))
	run := ScopeRun{Agent: "private", ProviderKey: "debug", Model: "allowed", Workspace: "/work/a", Config: cfg, Policy: locked}
	if _, _, err := AdmitSessionScope(ctx, store, &core.Request{SessionID: sess.ID}, run); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, core.UserText("history"), -1)); err != nil {
		t.Fatal(err)
	}

	// After a restart the agent definition was widened to another provider;
	// the runtime's own chain no longer carries the saved rule. The merged
	// durable policy still refuses the runtime's model before any egress.
	widened := ScopeRun{Agent: "private", ProviderKey: "other", Model: "denied", Workspace: "/work/a", Config: cfg,
		Policy: modelpolicy.Lock(cfg, modelpolicy.Policy{}.With("private", []string{"debug:*", "other:*"}))}
	gotCtx, _, err := AdmitSessionScope(ctx, store, &core.Request{SessionID: sess.ID}, widened)
	if err == nil || core.LockedSessionFromContext(core.ContextWithSessionID(gotCtx, sess.ID)) != "" {
		t.Fatalf("widened runtime admission = %v", err)
	}
	if _, _, err := AdmitSessionScope(ctx, store, &core.Request{SessionID: sess.ID, Model: "denied"}, run); err == nil {
		t.Fatal("request model outside the locked policy admitted")
	}
	saved, _ := store.Get(ctx, sess.ID)
	if err := saved.ModelPolicy.Check("private", "other", "denied"); err == nil {
		t.Fatal("refused admission widened the persisted policy")
	}

	// A runtime bound to a different workspace is refused once history exists.
	moved := run
	moved.Workspace = "/work/b"
	if _, _, err := AdmitSessionScope(ctx, store, &core.Request{SessionID: sess.ID}, moved); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("moved workspace admission = %v", err)
	}

	// Unlocked runtimes fail closed when the row cannot be read.
	if _, _, err := AdmitSessionScope(ctx, failingGetStore{&session.NoopStore{}}, &core.Request{SessionID: "x"}, ScopeRun{Agent: "plain"}); err == nil {
		t.Fatal("unreadable row admitted to an unlocked runtime")
	}
	// A detached (stateless, unpersisted) unlocked run keeps working; a
	// detached locked run still needs the store.
	if _, _, err := AdmitSessionScope(ctx, failingGetStore{&session.NoopStore{}}, &core.Request{SessionID: "x"}, ScopeRun{Agent: "plain", Detached: true}); err != nil {
		t.Fatalf("detached unlocked run refused: %v", err)
	}
	if _, _, err := AdmitSessionScope(ctx, failingGetStore{&session.NoopStore{}}, &core.Request{SessionID: "x"}, ScopeRun{Agent: "private", Detached: true, Policy: locked}); err == nil {
		t.Fatal("detached locked run admitted without atomic admission")
	}
}
