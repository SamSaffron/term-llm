package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/session/scopedb"
)

// Not parallel: it redirects the process-wide raw debug sink to a file.
func TestLockedRunNeverWritesRawDebugTranscript(t *testing.T) {
	f := newScopeFixture(t)
	plain := writeScopeAgent(t, filepath.Dir(f.private), "plaindebug", "name: plaindebug\nprovider: debug\nmodel: allowed\n")
	logDir := t.TempDir()
	closeLog, err := runtimeoutput.Start(logDir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	run := func(agent, marker string) *mockprovider.MockProvider {
		provider := mockprovider.NewMockProvider("debug").AddTextResponse("reply")
		includeTools := false
		runner := newCmdRunner(f.cfg, cmdRunnerOptions{Store: f.store, Debug: true, DebugRaw: true, ErrWriter: io.Discard})
		if _, err := runner.Run(context.Background(), runpkg.Request{
			Platform: runpkg.PlatformConsole, SessionID: session.NewID(), AgentName: agent, Persist: true, Stateful: true,
			Debug: true, DebugRaw: true, Messages: []core.Message{core.UserText(marker)}, IncludeConfiguredTools: &includeTools, ProviderInstance: provider,
		}, eventSinkFunc(func(core.Event) {})); err != nil {
			t.Fatalf("%s run: %v", agent, err)
		}
		return provider
	}
	locked := run(f.private, "invented-locked-raw-marker")
	for i, req := range locked.RecordedRequests() {
		if req.Debug || req.DebugRaw {
			t.Fatalf("locked provider request %d kept debug flags", i)
		}
	}
	// Control: the same host flags still log an ordinary run, so the sink works.
	run(plain, "invented-plain-raw-marker")
	closeLog()
	logged, err := os.ReadFile(filepath.Join(logDir, "tui.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "invented-locked-raw-marker") {
		t.Fatal("raw debug output contains the locked transcript")
	}
	if !strings.Contains(string(logged), "invented-plain-raw-marker") {
		t.Fatal("control run did not reach the raw debug sink; test cannot observe logging")
	}
}

// A live locked runtime refuses side questions even before its row shows the
// lock (first admission not yet committed).
func TestSideQuestionRefusesLiveLockedRuntimeBeforePersist(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	id := session.NewID()
	if err := f.store.Create(ctx, &session.Session{ID: id, Provider: "debug", Model: "allowed", Agent: "private"}); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{store: f.store, cfgRef: f.cfg, sessionMgr: newServeSessionManager(time.Minute, 10, nil)}
	rt := &serveRuntime{modelPolicy: modelpolicy.Policy{LockSession: true}, scopeLocked: true}
	srv.sessionMgr.sessions[id] = rt
	if _, err := srv.runtimeForSideQuestion(ctx, id); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("side question on live locked runtime = %v, want scope refusal", err)
	}
	srv.store = nil
	if _, err := srv.runtimeForSideQuestion(ctx, id); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("store-less side question on live locked runtime = %v, want scope refusal", err)
	}
}

func TestBranchHTTPOfLockedSessionKeepsIdentityAndPolicy(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	id := session.NewID()
	workspace := t.TempDir()
	f.privateProvider.AddTextResponse("reply")
	includeTools := false
	if _, err := newCmdRunner(f.cfg, cmdRunnerOptions{Store: f.store}).Run(ctx, runpkg.Request{
		Platform: runpkg.PlatformConsole, SessionID: id, AgentName: f.private, Persist: true, Stateful: true, Cwd: workspace,
		Messages: []core.Message{core.UserText("turn")}, IncludeConfiguredTools: &includeTools, ProviderInstance: f.privateProvider,
	}, eventSinkFunc(func(core.Event) {})); err != nil {
		t.Fatalf("locked first turn: %v", err)
	}
	parent, err := f.store.Get(ctx, id)
	if err != nil || parent.CWD == "" {
		t.Fatalf("parent = %+v, %v", parent, err)
	}
	project := &session.Project{Name: "scope", CanonicalDir: parent.CWD}
	if err := f.store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AssignSessionProject(ctx, id, project.ID, parent.CWD, parent.WorktreeDir); err != nil {
		t.Fatalf("first project assignment: %v", err)
	}
	if parent, err = f.store.Get(ctx, id); err != nil || parent.ProjectID != project.ID {
		t.Fatalf("parent after project = %+v, %v", parent, err)
	}
	messages, err := f.store.GetMessages(ctx, id, 0, 0)
	if err != nil || len(messages) == 0 {
		t.Fatalf("messages = %d, %v", len(messages), err)
	}
	anchor := messages[len(messages)-1].ID

	srv := &serveServer{store: f.store, cfgRef: f.cfg, sessionMgr: newServeSessionManager(time.Minute, 10, nil)}
	body, _ := json.Marshal(map[string]any{"anchor_message_id": anchor, "idempotency_key": "scope-branch"})
	out := httptest.NewRecorder()
	srv.handleCreateSessionBranch(out, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body))), id)
	if out.Code != http.StatusCreated {
		t.Fatalf("branch = %d %s", out.Code, out.Body.String())
	}
	var resp createSessionBranchResponse
	if err := json.Unmarshal(out.Body.Bytes(), &resp); err != nil || resp.Session.ID == "" || resp.Session.ID == id {
		t.Fatalf("branch response = %+v, %v", resp, err)
	}
	child, err := f.store.Get(ctx, resp.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Agent != parent.Agent || child.CWD != parent.CWD || child.WorktreeDir != parent.WorktreeDir || child.ProjectID != parent.ProjectID {
		t.Fatalf("branch identity = agent %q cwd %q worktree %q project %q; parent %q %q %q %q",
			child.Agent, child.CWD, child.WorktreeDir, child.ProjectID, parent.Agent, parent.CWD, parent.WorktreeDir, parent.ProjectID)
	}
	if !child.ModelPolicy.LockSession || !slices.EqualFunc(child.ModelPolicy.Rules, parent.ModelPolicy.Rules, func(a, b modelpolicy.Rule) bool {
		return a.Agent == b.Agent && slices.Equal(a.Allowed, b.Allowed)
	}) {
		t.Fatalf("branch policy = %+v, parent %+v", child.ModelPolicy, parent.ModelPolicy)
	}
	// The branch is as locked as its parent: another agent cannot take it over.
	before := len(f.unlockedProvider.RecordedRequests())
	if err := f.run(t, child.ID, f.other, f.unlockedProvider, "retarget"); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("branch retarget = %v, want scope refusal", err)
	}
	if got := len(f.unlockedProvider.RecordedRequests()); got != before {
		t.Fatalf("branch retarget dispatched %d provider request(s)", got-before)
	}
}

func TestBranchPathNotesRefusedForLockedSourceBeforeProvider(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	lockedID, plainID := session.NewID(), session.NewID()
	if err := f.run(t, lockedID, f.private, f.privateProvider, "invented private marker"); err != nil {
		t.Fatalf("locked turn: %v", err)
	}
	if err := f.run(t, plainID, f.other, f.unlockedProvider, "ordinary"); err != nil {
		t.Fatalf("ordinary turn: %v", err)
	}
	built := 0
	srv := &serveServer{store: f.store, cfgRef: f.cfg, sessionMgr: newServeSessionManager(time.Minute, 10, nil),
		pathNotesProviderFactory: func(string, string) (core.Provider, error) {
			built++
			return nil, errors.New("helper unavailable in test")
		}}
	source := []core.Message{core.UserText("invented private marker")}
	if note, status, msg := srv.generateBranchPathNote(ctx, lockedID, source, "summary", ""); note != nil || status != http.StatusConflict || !strings.Contains(msg, "locked") {
		t.Fatalf("locked path note = %v %d %q, want conflict refusal", note, status, msg)
	}
	if built != 0 {
		t.Fatalf("locked path note constructed %d helper provider(s)", built)
	}
	// Ordinary sessions still reach the helper; an empty mode still needs none.
	if _, status, _ := srv.generateBranchPathNote(ctx, plainID, source, "summary", ""); status == http.StatusConflict && built == 0 {
		t.Fatal("ordinary path note was refused")
	}
	if built != 1 {
		t.Fatalf("ordinary path note built %d helper provider(s), want 1", built)
	}
	if note, status, _ := srv.generateBranchPathNote(ctx, lockedID, source, "", ""); note != nil || status != 0 {
		t.Fatalf("empty mode = %v %d, want no helper", note, status)
	}
}
