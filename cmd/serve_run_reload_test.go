package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
)

type webContinuationTool struct{ calls atomic.Int32 }

func (*webContinuationTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "web_continuation_effect", Schema: map[string]any{"type": "object"}}
}
func (*webContinuationTool) Preview(json.RawMessage) string { return "effect" }
func (t *webContinuationTool) Execute(context.Context, json.RawMessage) (llm.ToolOutput, error) {
	t.calls.Add(1)
	return llm.TextOutput("once"), nil
}

func TestWebContinuationFailedExecResumesSameResponseAfterHello(t *testing.T) {
	old := restart.Default
	c := &restart.Coordinator{}
	restart.Default = c
	defer func() { restart.Default = old }()
	provider := llm.NewMockProvider("mock").AddTurn(llm.MockTurn{Text: "Hello", Usage: llm.Usage{InputTokens: 11, OutputTokens: 7}, ToolCalls: []llm.ToolCall{{ID: "effect-1", Name: "web_continuation_effect", Arguments: json.RawMessage(`{}`)}}}).AddTurn(llm.MockTurn{Text: "finished", Usage: llm.Usage{InputTokens: 13, OutputTokens: 5}})
	tool := &webContinuationTool{}
	registry := llm.NewToolRegistry()
	registry.Register(tool)
	rt := &serveRuntime{provider: provider, providerKey: "mock", engine: llm.NewEngine(provider, registry), defaultModel: "mock-model"}
	rt.Touch()
	srv := &serveServer{responseRuns: newServeResponseRunManager()}
	defer srv.responseRuns.Close()
	var requested atomic.Bool
	rt.responseCompletedCB = func(context.Context, int, llm.Message, llm.TurnMetrics) error {
		if requested.CompareAndSwap(false, true) {
			c.Request()
		}
		return nil
	}
	replaced := make(chan string, 1)
	stop, err := c.Bind(context.Background(), func(context.Context) error {
		srv.responseRuns.mu.Lock()
		defer srv.responseRuns.mu.Unlock()
		for _, run := range srv.responseRuns.runs {
			run.mu.Lock()
			saved := run.reloadContinuation
			run.mu.Unlock()
			if saved == nil || len(saved.Engine.Pending) != 1 || tool.calls.Load() != 0 {
				t.Error("replacement did not stop before the tool")
			}
			raw, err := json.Marshal(saved)
			if err != nil {
				t.Error(err)
			}
			var copy webRunContinuation
			if err := json.Unmarshal(raw, &copy); err != nil {
				t.Error(err)
			}
			if copy.CumulativeUsage.InputTokens != 11 || copy.View.Usage.InputTokens != 11 {
				t.Error("lost usage at execution boundary", copy.CumulativeUsage, copy.View.Usage)
			}
			if copy.View.Id != run.id || copy.Engine == nil {
				t.Error("lost response identity/continuation")
			}
			replaced <- run.id
		}
		return errors.New("fixture exec failure")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	run, err := srv.startResponseRun(rt, true, false, []llm.Message{llm.UserText("work")}, llm.Request{SessionID: "web-suspend", Tools: []llm.ToolSpec{tool.Spec()}, MaxTurns: 4}, "web-suspend", startResponseRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-replaced:
		if id != run.id {
			t.Fatal("response ID changed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("response never suspended")
	}
	waitForServeCondition(t, 3*time.Second, func() bool { return stringValue(run.snapshot()["status"]) == "completed" }, "same response to finish after failed exec")
	if tool.calls.Load() != 1 {
		t.Fatal("tool replayed", tool.calls.Load())
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.usage.InputTokens != 24 || run.usage.OutputTokens != 12 || run.sessionUsage.InputTokens != 24 {
		t.Fatal("lost or duplicated usage across suspension", run.usage, run.sessionUsage)
	}
	created := 0
	for _, event := range run.events {
		if event.Event == "response.created" {
			created++
		}
		if event.Event == "response.failed" {
			t.Fatal("suspension terminalized response")
		}
	}
	if created != 1 {
		t.Fatal("response recreated during rollback", created)
	}
}

// A serialized handoff, including the original response.created and partial
// output, rather than a newly admitted request with an empty event log.
func webReloadFixture(t *testing.T, id, provider string) webRunContinuation {
	t.Helper()
	mgr := newServeResponseRunManager()
	defer mgr.Close()
	run := newResponseRun(id, "session_"+id, "", "mock-model", time.Now().Unix(), nil)
	run.idempotencyScope = "draft_" + id
	run.requestFingerprint = "fingerprint_" + id
	if _, _, err := mgr.createOrGetByIdempotency(run, "key_"+id); err != nil {
		t.Fatal(err)
	}
	if err := run.appendEvent("response.created", map[string]any{"response": map[string]any{"id": id}}); err != nil {
		t.Fatal(err)
	}
	if err := run.appendEvent("response.output_text.delta", map[string]any{"delta": "Hello", "output_index": 0}); err != nil {
		t.Fatal(err)
	}
	checkpoint := &llm.Continuation{Turn: 1, BaseMessageCount: 1, Request: llm.Request{SessionID: run.sessionID, Model: "mock-model", Messages: []llm.Message{llm.UserText("work"), llm.AssistantText("Hello")}, MaxTurns: 3}}
	saved, err := snapshotWebRun(run, newResponseRunStreamState("mock-model", ""), checkpoint, &serveRuntime{providerKey: provider}, true, startResponseRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	var copy webRunContinuation
	if err := json.Unmarshal(raw, &copy); err != nil {
		t.Fatal(err)
	}
	return copy
}

func newWebReloadTestServer(t *testing.T) *serveServer {
	t.Helper()
	srv := &serveServer{responseRuns: newServeResponseRunManager()}
	srv.sessionMgr = newServeSessionManager(time.Hour, 10, nil)
	srv.runtimeFactory = func(_ context.Context, provider, _ string) (*serveRuntime, error) {
		if provider == "missing" {
			return nil, errors.New("provider removed during upgrade")
		}
		mock := llm.NewMockProvider(provider).AddTextResponse("finished")
		rt := &serveRuntime{provider: mock, providerKey: provider, engine: llm.NewEngine(mock, nil), defaultModel: "mock-model"}
		rt.Touch()
		return rt, nil
	}
	t.Cleanup(func() { srv.responseRuns.Close(); srv.sessionMgr.Close() })
	return srv
}

func assertReloadEventIdentity(t *testing.T, run *responseRun, saved webRunContinuation, terminal string) {
	t.Helper()
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.runEpoch != saved.View.RunEpoch || run.id != saved.View.Id {
		t.Fatalf("response identity changed: epoch=%d id=%s", run.runEpoch, run.id)
	}
	created, ended := 0, 0
	for i, event := range run.events {
		var payload struct {
			ResponseID string `json:"response_id"`
			Epoch      int64  `json:"run_epoch"`
			Sequence   int64  `json:"sequence_number"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ResponseID != saved.View.Id || payload.Epoch != saved.View.RunEpoch || payload.Sequence != int64(i+1) {
			t.Fatalf("mixed identity or event gap across reload: %+v", payload)
		}
		if event.Event == "response.created" {
			created++
		}
		if event.Event == terminal {
			ended++
		}
	}
	if created != 1 || ended != 1 {
		t.Fatalf("created=%d terminal=%d", created, ended)
	}
}

func TestWebReloadSuccessorPreservesEpochAndIdempotency(t *testing.T) {
	srv := newWebReloadTestServer(t)
	saved := webReloadFixture(t, "resp_resume", "mock")
	srv.restoreWebRuns(webReloadState{Runs: []webRunContinuation{saved}})
	run, found, err := srv.responseRuns.getByIdempotencyClaim(saved.View.IdempotencyScope, saved.View.IdempotencyKey, saved.View.RequestFingerprint)
	if err != nil || !found {
		t.Fatalf("original POST cannot find its response: found=%v err=%v", found, err)
	}
	waitForServeCondition(t, 3*time.Second, func() bool { return stringValue(run.snapshot()["status"]) == "completed" }, "restored response to finish")
	assertReloadEventIdentity(t, run, saved, "response.completed")
	if _, _, err := srv.responseRuns.getByIdempotencyClaim(saved.View.IdempotencyScope, saved.View.IdempotencyKey, "different request"); !errors.Is(err, errResponseRunKeyConflict) {
		t.Fatalf("lost fingerprint protection: %v", err)
	}
	if err := srv.responseRuns.restore(restoreWebRun(&saved, nil)); err == nil {
		t.Fatal("duplicate restoration overwrote the existing response")
	}
	// Restoring older epochs must not regress the counter for new admissions.
	newer := restoreWebRun(&saved, nil)
	newer.id = "resp_newer"
	newer.idempotencyKey = ""
	newer.runEpoch = saved.View.RunEpoch + 100
	if err := srv.responseRuns.restore(newer); err != nil {
		t.Fatal(err)
	}
	next := newResponseRun("resp_next", saved.View.SessionID, "", "mock-model", time.Now().Unix(), nil)
	if err := srv.responseRuns.create(next); err != nil {
		t.Fatal(err)
	}
	if next.runEpoch <= newer.runEpoch {
		t.Fatal("new response reused an old epoch")
	}
}

func TestWebReloadIsolatesUnrestorableResponses(t *testing.T) {
	for _, invalidEngine := range []bool{false, true} {
		t.Run(fmt.Sprint("invalid_engine=", invalidEngine), func(t *testing.T) {
			srv := newWebReloadTestServer(t)
			bad := webReloadFixture(t, "resp_bad", "missing")
			if invalidEngine {
				bad.Engine = nil
			}
			good := webReloadFixture(t, "resp_good", "mock")
			srv.restoreWebRuns(webReloadState{Runs: []webRunContinuation{bad, good}})
			for _, expected := range []struct {
				saved  webRunContinuation
				status string
			}{{bad, "failed"}, {good, "completed"}} {
				run, found, err := srv.responseRuns.getByIdempotencyClaim(expected.saved.View.IdempotencyScope, expected.saved.View.IdempotencyKey, expected.saved.View.RequestFingerprint)
				if err != nil || !found {
					t.Fatalf("lost restored response: %v %v", found, err)
				}
				waitForServeCondition(t, 3*time.Second, func() bool { return stringValue(run.snapshot()["status"]) == expected.status }, "each restored response to settle independently")
				assertReloadEventIdentity(t, run, expected.saved, "response."+expected.status)
			}
		})
	}
}

func TestWebReloadFailureRespectsDurableFence(t *testing.T) {
	for _, orphaned := range []bool{false, true} {
		t.Run(fmt.Sprint("orphaned=", orphaned), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sessions.db")
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			saved := webReloadFixture(t, "resp_fenced", "mock")
			if err := store.Create(ctx, &session.Session{ID: saved.View.SessionID, Provider: "mock", Model: "mock-model", Mode: session.ModeChat}); err != nil {
				t.Fatal(err)
			}
			lease, err := store.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: saved.View.Id, SessionID: saved.View.SessionID, OwnerInstanceID: "original-owner", RunEpoch: saved.View.RunEpoch, StartedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			saved.View.OwnerInstanceID = "original-owner"
			saved.View.FencingToken = lease.FencingToken
			// Exercise real lease expiry, not a mock of the renewal error.
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec("UPDATE serve_response_lifecycle SET lease_expires_at=? WHERE response_id=?", time.Now().Add(-time.Minute).UnixMilli(), saved.View.Id); err != nil {
				t.Fatal(err)
			}
			if orphaned {
				if _, err := store.RecoverExpiredResponseRuns(ctx, 10); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.RenewResponseRunLease(ctx, saved.View.Id, "original-owner", lease.FencingToken); !errors.Is(err, session.ErrResponseRunLeaseLost) {
				t.Fatalf("fixture lease has not expired: %v", err)
			}
			srv := newWebReloadTestServer(t)
			srv.store = store
			srv.responseOwnerOnce.Do(func() { srv.responseOwnerInstanceID = "original-owner" })
			srv.failWebRunRestore(&saved)
			run, found, _ := srv.responseRuns.getByIdempotencyClaim(saved.View.IdempotencyScope, saved.View.IdempotencyKey, saved.View.RequestFingerprint)
			if !found {
				t.Fatal("failed response is not replayable")
			}
			assertReloadEventIdentity(t, run, saved, "response.failed")
			var state string
			if err := db.QueryRow("SELECT state FROM serve_response_lifecycle WHERE response_id=?", saved.View.Id).Scan(&state); err != nil {
				t.Fatal(err)
			}
			want := "failed"
			if orphaned {
				want = "orphaned"
			}
			if state != want {
				t.Fatalf("durable ownership overwritten: got %s want %s", state, want)
			}
		})
	}
}

func TestWebReloadExpiredLeaseDoesNotStopOtherRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	bad := webReloadFixture(t, "resp_expired", "mock")
	good := webReloadFixture(t, "resp_owned", "mock")
	for _, saved := range []*webRunContinuation{&bad, &good} {
		if err := store.Create(ctx, &session.Session{ID: saved.View.SessionID, Provider: "mock", Model: "mock-model", Mode: session.ModeChat}); err != nil {
			t.Fatal(err)
		}
		lease, err := store.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: saved.View.Id, SessionID: saved.View.SessionID, OwnerInstanceID: "handoff-owner", RunEpoch: saved.View.RunEpoch, StartedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		saved.View.OwnerInstanceID = "handoff-owner"
		saved.View.FencingToken = lease.FencingToken
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE serve_response_lifecycle SET lease_expires_at=? WHERE response_id=?", time.Now().Add(-time.Minute).UnixMilli(), bad.View.Id); err != nil {
		t.Fatal(err)
	}
	srv := newWebReloadTestServer(t)
	srv.store = store
	srv.shutdownCh = make(chan struct{}) // Exercise real renewal in startResponseRun.
	t.Cleanup(func() {
		if srv.responseLifecycleCancel != nil {
			srv.responseLifecycleCancel()
			srv.responseLifecycleWG.Wait()
		}
	})
	srv.restoreWebRuns(webReloadState{Owner: "handoff-owner", Runs: []webRunContinuation{bad, good}})
	for _, expected := range []struct {
		saved  webRunContinuation
		status string
	}{{bad, "failed"}, {good, "completed"}} {
		run, found, _ := srv.responseRuns.getByIdempotencyClaim(expected.saved.View.IdempotencyScope, expected.saved.View.IdempotencyKey, expected.saved.View.RequestFingerprint)
		if !found {
			t.Fatal("response disappeared", expected.saved.View.Id)
		}
		waitForServeCondition(t, 3*time.Second, func() bool { return stringValue(run.snapshot()["status"]) == expected.status }, "lease outcomes to settle independently")
		assertReloadEventIdentity(t, run, expected.saved, "response."+expected.status)
	}
}
