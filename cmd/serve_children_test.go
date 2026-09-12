package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestChildReadRevisionIsSafeForProductionEpochs(t *testing.T) {
	productionEpoch := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC).UnixMicro()
	if got := safeChildRevision(productionEpoch); got < 0 || got > maxChildReadRevision {
		t.Fatalf("safeChildRevision(%d) = %d", productionEpoch, got)
	}
	if got := terminalChildRevision(maxChildReadRevision); got != maxChildReadRevision {
		t.Fatalf("terminal revision overflowed: %d", got)
	}
	active := safeChildRevision(time.Now().UnixMilli())
	if terminal := terminalChildRevision(active); terminal <= active {
		t.Fatalf("terminal revision %d is not newer than active %d", terminal, active)
	}
}

func TestHandleSessionChildrenReturnsBoundedAuthoritativeProjection(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent := &session.Session{ID: "parent", Provider: "debug", Model: "fast", Mode: session.ModeChat}
	if err := store.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := &session.Session{
		ID: "child", Provider: "debug", Model: "fast", Mode: session.ModeChat,
		ParentID: parent.ID, IsSubagent: true, Agent: "reviewer", Status: session.StatusComplete,
	}
	if err := store.Create(ctx, child); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateMetrics(ctx, child.ID, 2, 3, 11, 7, 13, 5); err != nil {
		t.Fatal(err)
	}
	if storedChild, getErr := store.Get(ctx, child.ID); getErr != nil || storedChild.ParentID != parent.ID {
		t.Fatalf("stored child = %#v, err = %v", storedChild, getErr)
	}
	if err := store.AddMessage(ctx, child.ID, &session.Message{
		Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "Review the concurrency boundary"}},
		TextContent: "Review the concurrency boundary",
	}); err != nil {
		t.Fatal(err)
	}
	spawnCall := session.NewMessage(parent.ID, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
		Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "spawn-1", Name: tools.SpawnAgentToolName},
	}}}, -1)
	if err := store.AddMessage(ctx, parent.ID, spawnCall); err != nil {
		t.Fatal(err)
	}
	spawnResult, _ := json.Marshal(tools.SpawnAgentResult{AgentName: "reviewer", SessionID: child.ID})
	if err := store.AddMessage(ctx, parent.ID, session.NewMessage(
		parent.ID,
		llm.ToolResultMessage("spawn-1", tools.SpawnAgentToolName, string(spawnResult), nil),
		-1,
	)); err != nil {
		t.Fatal(err)
	}
	parentMessages, err := store.GetMessages(ctx, parent.ID, 1, 0)
	if err != nil || len(parentMessages) != 1 {
		t.Fatalf("parent messages = %#v, err = %v", parentMessages, err)
	}
	spawnItemID := parentMessages[0].ID
	unrelated := &session.Session{ID: "unrelated", Provider: "debug", Model: "fast", Mode: session.ModeChat}
	if err := store.Create(ctx, unrelated); err != nil {
		t.Fatal(err)
	}

	listed, listErr := store.List(ctx, session.ListOptions{ParentID: parent.ID, Limit: maxChildRunProjection, SortByActivity: true})
	if listErr != nil || len(listed) != 1 {
		t.Fatalf("listed children = %#v, err = %v", listed, listErr)
	}

	runs := newServeResponseRunManager()
	defer runs.Close()
	srv := &serveServer{store: store, responseRuns: runs}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/children", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionChildren(rr, req, parent.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		ParentSessionID string               `json:"parent_session_id"`
		Revision        int64                `json:"revision"`
		Children        []childRunProjection `json:"children"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ParentSessionID != parent.ID || payload.Revision <= 0 || len(payload.Children) != 1 {
		t.Fatalf("projection = %#v", payload)
	}
	got := payload.Children[0]
	if got.InputTokens != 11 || got.OutputTokens != 7 || got.CachedInputTokens != 13 || got.CacheWriteTokens != 5 || got.Model != "fast" {
		t.Fatalf("child metrics = %+v", got)
	}
	selected, err := srv.selectedWebSession(ctx, child.ID, nil)
	if err != nil || selected == nil || selected.Metrics == nil || selected.Metrics.InputTokens != 11 {
		t.Fatalf("selected metrics = %+v, err = %v", selected, err)
	}
	if got.SessionID != child.ID || got.ParentSessionID != parent.ID || got.ParentSpawnItemID != spawnItemID || got.ParentSpawnCallID != "spawn-1" || got.TaskSummary != "Review the concurrency boundary" || got.StartedAt <= 0 || got.EndedAt <= 0 {
		t.Fatalf("child projection = %#v", got)
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	conditional := httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/children", nil)
	conditional.Header.Set("If-None-Match", etag)
	conditionalResult := httptest.NewRecorder()
	srv.handleSessionChildren(conditionalResult, conditional, parent.ID)
	if conditionalResult.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", conditionalResult.Code)
	}
}

func TestRuntimeSpawnPersistsChildForStats(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		job, contextParent, cancel bool
	}{
		{name: "web"},
		{name: "web_context_parent", contextParent: true},
		{name: "job_owned_store", job: true},
		{name: "cancelled_job", job: true, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dbPath := filepath.Join(t.TempDir(), "sessions.db")
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: dbPath})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			parent := &session.Session{ID: "web-parent", Provider: "debug", Model: "fast", Mode: session.ModeChat}
			if err := store.Create(ctx, parent); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			agentDir := filepath.Join(root, "stats-child")
			if err := os.MkdirAll(agentDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte("name: stats-child\nmodel: debug:fast\nskills: none\nmax_turns: 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "fast", FastModel: "fast"}}, Agents: config.AgentsConfig{SearchPaths: []string{root}}}
			defaults := serveRuntimeRunnerDefaults(serveAgentRuntimeOptions{cfg: cfg, cmd: &cobra.Command{}, store: store}, serveRuntimeRequest{SessionID: parent.ID}, tools.ModePrompt)
			cfg.Sessions = config.SessionsConfig{Enabled: true, Path: dbPath}
			platform := runpkg.PlatformWeb
			requestParent := parent.ID
			if tc.contextParent {
				requestParent = ""
			}
			if tc.job {
				defaults = serveJobsRunnerOptions(resolvedApprovalMode{Mode: tools.ModePrompt})
				platform = runpkg.PlatformJob
			}
			defaults.Tools = "spawn_agent"
			defaults.ToolsSet = true
			runner := newCmdRunner(cfg, defaults).(*cmdRunner)
			env, err := runner.prepare(ctx, runpkg.Request{Platform: platform, SessionID: requestParent, Persist: tc.job, Provider: "debug", Model: "fast", Cwd: t.TempDir(), DeferSession: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			spawn := env.runtime.toolMgr.GetSpawnAgentTool()
			if spawn == nil {
				t.Fatal("spawn tool missing")
			}
			toolCtx := llm.ContextWithCallID(llm.ContextWithSessionID(ctx, parent.ID), "spawn-stats")
			var output llm.ToolOutput
			if tc.cancel {
				output = executeCancelledStatsChild(t, toolCtx, cancel, env, spawn)
			} else {
				output, err = spawn.Execute(toolCtx, json.RawMessage(`{"agent_name":"stats-child","prompt":"Say hello","model":"debug:fast"}`))
			}
			if err != nil {
				t.Fatal(err)
			}
			var result tools.SpawnAgentResult
			if err := json.Unmarshal([]byte(output.Content), &result); err != nil || result.SessionID == "" || (!tc.cancel && result.Error != "") {
				t.Fatalf("spawn result = %s, err = %v", output.Content, err)
			}
			// Job-owned stores must flush child status before cleanup closes their handle.
			env.Close()
			child, err := store.Get(context.Background(), result.SessionID)
			if err != nil || child == nil {
				t.Fatalf("returned child ID has no persisted session: %q, err = %v", result.SessionID, err)
			}
			if child.ParentID != parent.ID || (!tc.cancel && child.OutputTokens == 0) {
				t.Fatalf("child metadata/usage = %+v", child)
			}
			wantState := session.StatusComplete
			if tc.cancel {
				wantState = session.StatusInterrupted
			}
			if child.Status != wantState {
				t.Fatalf("child state = %q, want %q", child.Status, wantState)
			}
			srv := &serveServer{store: store}
			rr := httptest.NewRecorder()
			srv.handleSessionChildren(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions/web-parent/children", nil), parent.ID)
			var payload struct {
				Children []childRunProjection `json:"children"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if rr.Code != http.StatusOK || len(payload.Children) != 1 || payload.Children[0].SessionID != child.ID || payload.Children[0].OutputTokens != child.OutputTokens || payload.Children[0].ParentSessionID != parent.ID || payload.Children[0].State != wantState || payload.Children[0].EndedAt < payload.Children[0].StartedAt || payload.Children[0].EndedAt == 0 {
				t.Fatalf("web stats projection = %s", rr.Body.String())
			}
		})
	}
}

// Hold the child in its event callback while the parent closes, reproducing the
// engine's cancellation behavior: it need not wait for tool goroutines to exit.
func executeCancelledStatsChild(t *testing.T, ctx context.Context, cancel context.CancelFunc, env *cmdRunEnvironment, spawn *tools.SpawnAgentTool) llm.ToolOutput {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	spawn.SetEventCallback(func(_ string, event tools.SubagentEvent) {
		if event.Type == tools.SubagentEventText {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}
	})
	type outcome struct {
		output llm.ToolOutput
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		output, err := spawn.Execute(ctx, json.RawMessage(`{"agent_name":"stats-child","prompt":"Say hello","model":"debug:fast"}`))
		finished <- outcome{output, err}
	}()
	watchdog, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	select {
	case <-entered:
	case <-watchdog.Done():
		t.Fatal("child did not start")
	}
	cancel()
	closed := make(chan struct{})
	go func() { env.Close(); close(closed) }()
	// Wait for the drain barrier rather than using a timing sleep. If Close skips
	// draining, the closed channel exposes that failure while the child is held.
	for {
		select {
		case <-closed:
			t.Fatal("parent closed its store before child finished")
		case <-watchdog.Done():
			t.Fatal("parent did not start draining children")
		default:
		}
		runner := env.runtime.spawnRunner
		if runner == nil {
			t.Fatal("runtime does not own its spawn runner")
		}
		runner.runMu.Lock()
		draining := runner.draining
		runner.runMu.Unlock()
		if draining {
			break
		}
		runtime.Gosched()
	}
	unblock()
	var result outcome
	select {
	case result = <-finished:
	case <-watchdog.Done():
		t.Fatal("cancelled child did not exit")
	}
	select {
	case <-closed:
	case <-watchdog.Done():
		t.Fatal("parent close did not finish after child drained")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	return result.output
}
