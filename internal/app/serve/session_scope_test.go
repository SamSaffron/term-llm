package serve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/session/scopedb"
)

type scopeFixture struct {
	cfg              *config.Config
	store            *session.SQLiteStore
	private, other   string
	privateProvider  *mockprovider.MockProvider
	unlockedProvider *mockprovider.MockProvider
}

func writeScopeAgent(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newScopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &scopeFixture{
		cfg: &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{
			"debug": {Model: "allowed"}, "other": {Model: "denied"},
		}, Agents: config.AgentsConfig{SearchPaths: []string{root}}},
		store:            store,
		private:          writeScopeAgent(t, root, "private", "name: private\nprovider: debug\nmodel: allowed\nallowed_models: [debug:allowed]\nlock_session: true\n"),
		other:            writeScopeAgent(t, root, "other", "name: other\nprovider: other\nmodel: denied\n"),
		privateProvider:  mockprovider.NewMockProvider("debug"),
		unlockedProvider: mockprovider.NewMockProvider("other"),
	}
}

func (f *scopeFixture) run(t *testing.T, id, agent string, provider *mockprovider.MockProvider, text string) error {
	t.Helper()
	provider.AddTextResponse("reply")
	includeTools := false
	runner := newCmdRunner(f.cfg, cmdRunnerOptions{Store: f.store})
	_, err := runner.Run(context.Background(), runpkg.Request{
		Platform: runpkg.PlatformConsole, SessionID: id, AgentName: agent, Persist: true, Stateful: true,
		Cwd: "", Messages: []core.Message{core.UserText(text)}, IncludeConfiguredTools: &includeTools, ProviderInstance: provider,
	}, eventSinkFunc(func(core.Event) {}))
	return err
}

func TestLockedSessionPersistsFullPolicyAndRefusesRetarget(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	id := session.NewID()
	if err := f.run(t, id, f.private, f.privateProvider, "invented private marker"); err != nil {
		t.Fatalf("locked first turn: %v", err)
	}
	saved, err := f.store.Get(ctx, id)
	if err != nil || !saved.ModelPolicy.LockSession || !saved.ModelPolicy.Restricted() || saved.Agent != f.private {
		t.Fatalf("persisted session = %+v, %v", saved, err)
	}
	// The gap observed on stock builds: a restarted host retargets the agent.
	// A fresh runner (no warm runtime) models the restart.
	if err := f.run(t, id, f.other, f.unlockedProvider, "after restart"); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("retarget error = %v, want scope refusal", err)
	}
	if got := len(f.unlockedProvider.RecordedRequests()); got != 0 {
		t.Fatalf("disallowed provider received %d request(s)", got)
	}
	// Even if the stored agent definition later drops its restriction, the
	// persisted lock keeps the conversation on its admitted policy.
	after, _ := f.store.Get(ctx, id)
	if after.Agent != f.private || !after.ModelPolicy.LockSession {
		t.Fatalf("session identity changed: %+v", after)
	}
	if err := after.ModelPolicy.Check(f.private, "other", "denied"); err == nil {
		t.Fatal("persisted locked policy allows the disallowed model")
	}
	// Same-agent continuation still works.
	if err := f.run(t, id, "", f.privateProvider, "continue"); err != nil {
		t.Fatalf("same-agent continuation: %v", err)
	}
}

func TestUnlockedSessionBehaviourUnchanged(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	id := session.NewID()
	if err := f.run(t, id, f.other, f.unlockedProvider, "ordinary"); err != nil {
		t.Fatalf("ordinary turn: %v", err)
	}
	saved, err := f.store.Get(ctx, id)
	if err != nil || saved.ModelPolicy.LockSession || saved.ModelPolicy.Restricted() {
		t.Fatalf("ordinary session policy = %+v, %v", saved, err)
	}
	// An unlocked conversation with history cannot be retrofitted into a lock.
	if err := f.run(t, id, f.private, f.privateProvider, "retrofit"); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("retrofit error = %v, want scope refusal", err)
	}
	if got := len(f.privateProvider.RecordedRequests()); got != 0 {
		t.Fatalf("retrofit reached provider %d time(s)", got)
	}
}

func TestLockedSessionSurvivesDefinitionWideningAndAuxRouting(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	id := session.NewID()
	if err := f.run(t, id, f.private, f.privateProvider, "invented private marker"); err != nil {
		t.Fatalf("locked first turn: %v", err)
	}
	// The operator later widens the definition and removes lock_session.
	widened := "name: private\nprovider: debug\nmodel: allowed\nallowed_models: [debug:*, other:*]\n"
	if err := os.WriteFile(filepath.Join(f.private, "agent.yaml"), []byte(widened), 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := f.store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := appruntime.SessionModelPolicy(f.cfg, saved)
	if err != nil || !policy.LockSession {
		t.Fatalf("session policy after widening = %+v, %v", policy, err)
	}
	if err := policy.CheckWithConfig(f.cfg, f.private, "other", "denied"); err == nil {
		t.Fatal("widened definition widened the locked conversation")
	}
	// A resumed turn that asks for the newly "allowed" model is refused before dispatch.
	f.unlockedProvider.AddTextResponse("must not run")
	includeTools := false
	_, err = newCmdRunner(f.cfg, cmdRunnerOptions{Store: f.store}).Run(ctx, runpkg.Request{
		Platform: runpkg.PlatformConsole, SessionID: id, Provider: "other:denied", Persist: true, Stateful: true, Resume: true,
		Messages: []core.Message{core.UserText("switch")}, IncludeConfiguredTools: &includeTools, ProviderInstance: f.unlockedProvider,
	}, eventSinkFunc(func(core.Event) {}))
	if err == nil {
		t.Fatal("resume onto a disallowed provider admitted")
	}
	if got := len(f.unlockedProvider.RecordedRequests()); got != 0 {
		t.Fatalf("disallowed provider received %d request(s)", got)
	}
	// Auxiliary calls (titles, interrupt classification) route only to allowed models.
	f.cfg.Providers["debug"] = config.ProviderConfig{Model: "allowed", FastProvider: "other", FastModel: "denied"}
	aux, err := appruntime.AuxProviderForPolicy(f.cfg, policy, appruntime.ModelSelection{Provider: "debug", Model: "allowed"}, "debug")
	if err != nil || aux == nil {
		t.Fatalf("aux provider = %v, %v", aux, err)
	}
	if name := aux.Name(); strings.Contains(name, "denied") {
		t.Fatalf("aux routed to disallowed model %q", name)
	}
}

func offeredToolNames(req core.Request) []string {
	names := make([]string, 0, len(req.Tools))
	for _, spec := range req.Tools {
		names = append(names, spec.Name)
	}
	return names
}

func TestLockedAgentSearchSettingCannotBeWidened(t *testing.T) {
	f := newScopeFixture(t)
	root := filepath.Dir(f.private)
	locked := writeScopeAgent(t, root, "lockednosearch", "name: lockednosearch\nprovider: debug\nmodel: allowed\nallowed_models: [debug:allowed]\nlock_session: true\ntools:\n  enabled: [update_plan]\n")
	plain := writeScopeAgent(t, root, "plainnosearch", "name: plainnosearch\nprovider: debug\nmodel: allowed\ntools:\n  enabled: [update_plan]\n")
	search := true
	run := func(agent string, provider *mockprovider.MockProvider) error {
		_, err := newCmdRunner(f.cfg, cmdRunnerOptions{Store: f.store}).Run(context.Background(), runpkg.Request{
			Platform: runpkg.PlatformConsole, SessionID: session.NewID(), AgentName: agent, Persist: true, Stateful: true,
			// A request/UI search toggle asks for search the agent does not enable.
			Search: &search, Messages: []core.Message{core.UserText("look this up")}, ProviderInstance: provider,
		}, eventSinkFunc(func(core.Event) {}))
		return err
	}

	// Locked: neither offered nor callable, even when the model names it.
	lockedProvider := mockprovider.NewMockProvider("debug").
		AddToolCall("call-1", core.WebSearchToolName, map[string]string{"query": "invented"}).
		AddTextResponse("done")
	if err := run(locked, lockedProvider); err != nil {
		t.Fatalf("locked run: %v", err)
	}
	requests := lockedProvider.RecordedRequests()
	if len(requests) == 0 {
		t.Fatal("locked run made no provider request")
	}
	for i, req := range requests {
		for _, name := range offeredToolNames(req) {
			if name == core.WebSearchToolName || name == core.ReadURLToolName {
				t.Fatalf("locked request %d offered %s: %v", i, name, offeredToolNames(req))
			}
		}
		if req.Search {
			t.Fatalf("locked request %d enabled native search", i)
		}
		for _, msg := range req.Messages {
			for _, part := range msg.Parts {
				if part.ToolResult != nil && part.ToolResult.ID == "call-1" && !strings.Contains(part.ToolResult.Content, "not registered") {
					t.Fatalf("unoffered web_search executed: %q", part.ToolResult.Content)
				}
			}
		}
	}
	// The host never executed it: an unregistered call ends the server-side
	// turn instead of producing a search result for a follow-up request.
	if len(requests) != 1 {
		t.Fatalf("locked run made %d provider requests, want 1", len(requests))
	}

	// Ordinary unlocked behaviour is unchanged: the request toggle still applies.
	plainProvider := mockprovider.NewMockProvider("debug").AddTextResponse("done")
	if err := run(plain, plainProvider); err != nil {
		t.Fatalf("unlocked run: %v", err)
	}
	if names := offeredToolNames(plainProvider.RecordedRequests()[0]); !slices.Contains(names, core.WebSearchToolName) {
		t.Fatalf("unlocked request lost search toggle: %v", names)
	}
}

func TestLockedSessionAuxiliaryPathsFailClosed(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	id := session.NewID()
	if err := f.run(t, id, f.private, f.privateProvider, "invented private marker"); err != nil {
		t.Fatalf("locked first turn: %v", err)
	}
	plainID := session.NewID()
	if err := f.run(t, plainID, f.other, f.unlockedProvider, "invented private marker"); err != nil {
		t.Fatalf("ordinary turn: %v", err)
	}
	srv := &serveServer{store: f.store, cfgRef: f.cfg, sessionMgr: newServeSessionManager(time.Minute, 10, nil)}
	before := len(f.privateProvider.RecordedRequests())

	if _, err := srv.runtimeForSideQuestion(ctx, id); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("side question = %v, want scope refusal", err)
	}
	out := httptest.NewRecorder()
	srv.handleSessionRuntimeCompact(out, httptest.NewRequest(http.MethodPost, "/", nil), id)
	if out.Code != http.StatusConflict || !strings.Contains(out.Body.String(), "locked") {
		t.Fatalf("manual compaction = %d %s", out.Code, out.Body.String())
	}
	if _, err := srv.resolveLiveSwitchTarget(ctx, id); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("voice switch = %v, want scope refusal", err)
	}
	if _, err := srv.resolveLiveSwitchTarget(ctx, plainID); err != nil {
		t.Fatalf("ordinary voice switch = %v", err)
	}
	// Other agents' session_directory never sees locked rows or snippets.
	for _, q := range []sessionDirectoryQuery{{}, {Query: "invented"}} {
		entries, err := srv.sessionDirectory(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		seenPlain := false
		for _, entry := range entries {
			if entry.ID == id {
				t.Fatalf("session_directory %+v exposed the locked session", q)
			}
			seenPlain = seenPlain || entry.ID == plainID
		}
		if !seenPlain {
			t.Fatalf("session_directory %+v lost the ordinary session: %+v", q, entries)
		}
	}
	if got := len(f.privateProvider.RecordedRequests()); got != before {
		t.Fatalf("auxiliary refusals dispatched %d provider request(s)", got-before)
	}
}

func TestLockedSessionRefusesWorkspaceRetargetAfterRestart(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	id := session.NewID()
	first, second := t.TempDir(), t.TempDir()
	run := func(cwd string, provider *mockprovider.MockProvider) error {
		provider.AddTextResponse("reply")
		includeTools := false
		_, err := newCmdRunner(f.cfg, cmdRunnerOptions{Store: f.store}).Run(ctx, runpkg.Request{
			Platform: runpkg.PlatformConsole, SessionID: id, AgentName: f.private, Persist: true, Stateful: true, Cwd: cwd,
			Messages: []core.Message{core.UserText("turn")}, IncludeConfiguredTools: &includeTools, ProviderInstance: provider,
		}, eventSinkFunc(func(core.Event) {}))
		return err
	}
	if err := run(first, f.privateProvider); err != nil {
		t.Fatalf("locked first turn: %v", err)
	}
	saved, _ := f.store.Get(ctx, id)
	if saved.CWD == "" {
		t.Fatal("locked first turn did not bind a workspace")
	}
	moved := mockprovider.NewMockProvider("debug")
	if err := run(second, moved); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("workspace retarget = %v, want scope refusal", err)
	}
	if got := len(moved.RecordedRequests()); got != 0 {
		t.Fatalf("retargeted workspace dispatched %d request(s)", got)
	}
	if after, _ := f.store.Get(ctx, id); after.CWD != saved.CWD {
		t.Fatalf("workspace changed from %q to %q", saved.CWD, after.CWD)
	}
}
