package serve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

func waitForErrorHistoryGuardRun(t *testing.T, run *responseRun, wantStatus string) {
	t.Helper()
	select {
	case <-run.settled:
	case <-time.After(5 * time.Second):
		t.Fatal("response did not settle")
	}
	if got := run.snapshot()["status"]; got != wantStatus {
		t.Fatalf("response status = %v, want %s", got, wantStatus)
	}
}

func TestResponseRunSetupErrorRetainsUnhydratedTranscript(t *testing.T) {
	for _, metadataOnly := range []bool{false, true} {
		name := "cold"
		if metadataOnly {
			name = "metadata-only"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			srv, store := newServeProjectTestServer(t)
			srv.responseRuns = newServeResponseRunManager()
			defer srv.responseRuns.Close()
			sess := &session.Session{ID: "setup-error-history", Provider: "mock", Model: "mock-model", Mode: session.ModeChat, CompactionSeq: -1}
			if err := store.Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			for _, msg := range []core.Message{core.UserText("original question"), core.AssistantText("original answer")} {
				if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
					t.Fatal(err)
				}
			}
			provider := mockprovider.NewMockProvider("mock").AddTextResponse("next answer")
			rt := &serveRuntime{provider: provider, engine: engine.NewEngine(provider, nil), store: store, defaultModel: "mock-model"}
			if metadataOnly {
				rt.sessionMeta = sess
			}
			run, err := srv.startResponseRun(rt, true, false, []core.Message{core.UserText("setup failed")}, core.Request{SessionID: sess.ID}, sess.ID, startResponseRunOptions{
				runtimeSetup: func(*core.Request) error { return errors.New("setup failed before hydration") },
			})
			if err != nil {
				t.Fatal(err)
			}
			waitForErrorHistoryGuardRun(t, run, "failed")
			if len(provider.RecordedRequests()) != 0 {
				t.Fatal("setup failure reached provider")
			}
			failedHistory, err := store.GetMessages(ctx, sess.ID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(failedHistory) != 3 {
				t.Fatalf("failed history length = %d, want 3", len(failedHistory))
			}
			if _, ok := llm.ParseRunErrorMarker(failedHistory[2].ToLLMMessage()); !ok {
				t.Fatal("setup error marker was not persisted")
			}
			if len(rt.history) != 0 || rt.historyPersisted {
				t.Fatal("setup error made an unhydrated history authoritative")
			}
			next, err := srv.startResponseRun(rt, true, false, []core.Message{core.UserText("next question")}, core.Request{SessionID: sess.ID}, sess.ID, startResponseRunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			waitForErrorHistoryGuardRun(t, next, "completed")
			requests := provider.RecordedRequests()
			if len(requests) != 1 || !requestContainsText(requests[0], "original question") || !requestContainsText(requests[0], "original answer") || !requestContainsText(requests[0], "next question") {
				t.Fatal("next turn did not hydrate the full durable conversation")
			}
			stored, err := store.GetMessages(ctx, sess.ID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(stored) != 5 {
				t.Fatalf("next history length = %d, want 5", len(stored))
			}
			for i, previous := range failedHistory {
				if stored[i].ID != previous.ID || stored[i].TextContent != previous.TextContent {
					t.Fatalf("next turn replaced durable transcript row %d", i)
				}
			}
		})
	}
}

func TestResponseRunBusyErrorSettlesWhileRuntimeLocked(t *testing.T) {
	ctx := context.Background()
	srv, store := newServeProjectTestServer(t)
	srv.responseRuns = newServeResponseRunManager()
	defer srv.responseRuns.Close()
	sess := &session.Session{ID: "busy-error-history", Provider: "mock", Model: "mock-model", Mode: session.ModeChat, CompactionSeq: -1}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	msg := core.UserText("active turn")
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
		t.Fatal(err)
	}
	provider := mockprovider.NewMockProvider("mock")
	rt := &serveRuntime{provider: provider, engine: engine.NewEngine(provider, nil), store: store, history: []core.Message{msg}, historyPersisted: true}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	run, err := srv.startResponseRun(rt, true, false, []core.Message{core.UserText("rejected turn")}, core.Request{SessionID: sess.ID}, sess.ID, startResponseRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately keep rt.mu held until after terminalization; no sleeps or
	// unlocking the active runtime to make the rejected run finish.
	waitForErrorHistoryGuardRun(t, run, "failed")
	if run.errorType != "conflict_error" {
		t.Fatalf("error type = %q, want conflict_error", run.errorType)
	}
	if len(provider.RecordedRequests()) != 0 {
		t.Fatal("busy runtime reached provider")
	}
	stored, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || len(rt.history) != 1 || !rt.historyPersisted {
		t.Fatal("busy rejection mutated the active turn's durable or in-memory transcript")
	}
}

func TestResponseRunProviderErrorWaitsForTransientRuntimeLock(t *testing.T) {
	ctx := context.Background()
	srv, store := newServeProjectTestServer(t)
	srv.responseRuns = newServeResponseRunManager()
	defer srv.responseRuns.Close()
	providerErr := errors.New("provider rejected the request")
	provider := mockprovider.NewMockProvider("mock").AddError(providerErr)
	rt := &serveRuntime{provider: provider, engine: engine.NewEngine(provider, nil), store: store, defaultModel: "mock-model"}
	const sessionID = "transient-lock-error-history"
	resultReady := make(chan error, 1)
	resumeResult := make(chan struct{})
	resumed := false
	defer func() {
		if !resumed {
			close(resumeResult)
		}
	}()
	run, err := srv.startResponseRun(rt, true, false, []core.Message{core.UserText("failed question")}, core.Request{SessionID: sessionID}, sessionID, startResponseRunOptions{
		onResult: func(_ serveRunResult, err error) {
			resultReady <- err
			<-resumeResult
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-resultReady:
		if !errors.Is(err, providerErr) {
			t.Fatalf("run error = %v, want provider error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider failure did not reach result callback")
	}
	// Simulate a snapshot reader acquiring rt.mu after the provider's run
	// releases it, but before the error is saved. This is not a busy rejection.
	rt.mu.Lock()
	locked := true
	defer func() {
		if locked {
			rt.mu.Unlock()
		}
	}()
	close(resumeResult)
	resumed = true
	select {
	case <-run.settled:
		t.Fatal("genuine error was terminalized without waiting to persist its marker")
	case <-time.After(100 * time.Millisecond):
	}
	stored, err := store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].TextContent != "failed question" {
		t.Fatal("error write did not wait for the runtime snapshot lock")
	}
	rt.mu.Unlock()
	locked = false
	waitForErrorHistoryGuardRun(t, run, "failed")
	stored, err = store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || len(rt.history) != 2 || !rt.historyPersisted {
		t.Fatal("genuine failure lost its durable or in-memory error marker")
	}
	for _, msg := range []core.Message{stored[1].ToLLMMessage(), rt.history[1]} {
		marker, ok := llm.ParseRunErrorMarker(msg)
		if !ok || marker.ResponseID != run.id || marker.Message != providerErr.Error() {
			t.Fatalf("unexpected error marker: %#v", marker)
		}
	}
}

func TestPersistResponseRunErrorReloadsDivergentHistory(t *testing.T) {
	ctx := context.Background()
	store := newServeRuntimeTestStore()
	sess := &session.Session{ID: "divergent-error-history", CompactionSeq: -1}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, core.UserText("durable question"), -1)); err != nil {
		t.Fatal(err)
	}
	rt := &serveRuntime{store: store, history: []core.Message{core.UserText("incomplete snapshot")}}
	(&serveServer{}).persistResponseRunErrorEvent(ctx, rt, sess.ID, "resp-divergent", "server_error", "failed")
	if rt.historyPersisted || len(rt.history) != 0 {
		t.Fatal("divergent memory was retained or marked persisted")
	}
	if !rt.restorePersistedHistory(ctx, sess) {
		t.Fatal("could not reload durable transcript")
	}
	if len(rt.history) != 2 || core.MessageText(rt.history[0]) != "durable question" || !rt.historyPersisted {
		t.Fatalf("reloaded history = %#v", rt.history)
	}
	if _, ok := llm.ParseRunErrorMarker(rt.history[1]); !ok {
		t.Fatal("reload lost saved error marker")
	}
}

func TestPersistResponseRunErrorFenceFailureLeavesMemoryUntouched(t *testing.T) {
	for _, validateFailure := range []bool{false, true} {
		name := "write"
		if validateFailure {
			name = "validation"
		}
		t.Run(name, func(t *testing.T) {
			store := newServeRuntimeTestStore()
			if !validateFailure {
				store.addMessageHook = func(context.Context, string, *session.Message) error { return session.ErrResponseRunLeaseLost }
			}
			rt := &serveRuntime{store: store, history: []core.Message{core.UserText("retained")}, historyPersisted: true}
			run := newResponseRun("resp-fenced-error", "sess-fenced-error", "", "mock", 0, nil)
			if validateFailure {
				run.validateLifecycle = func() error { return session.ErrResponseRunLeaseLost }
			}
			ctx := withResponseRunContext(context.Background(), run)
			(&serveServer{}).persistResponseRunErrorEvent(ctx, rt, run.sessionID, run.id, "server_error", "failed")
			if len(rt.history) != 1 || core.MessageText(rt.history[0]) != "retained" || !rt.historyPersisted {
				t.Fatal("failed fenced write changed memory")
			}
			stored, err := store.GetMessages(ctx, run.sessionID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(stored) != 0 {
				t.Fatal("failed fenced write persisted an error marker")
			}
			if !rt.mu.TryLock() {
				t.Fatal("failed fenced write leaked runtime lock")
			}
			rt.mu.Unlock()
		})
	}
}
