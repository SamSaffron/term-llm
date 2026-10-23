package serve

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestFailedModelSwapPublishesUsableContinuation(t *testing.T) {
	ctx := context.Background()
	srv, store := newServeProjectTestServer(t)
	srv.projectsEnabled = false
	srv.cfgRef = &config.Config{DefaultProvider: "old"}
	sess := &session.Session{ID: session.NewID(), Provider: "old", ProviderKey: "old", Model: "old-model", Mode: session.ModeChat, CompactionSeq: -1}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []core.Message{core.UserText("original question"), core.AssistantText("original answer")} {
		if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
			t.Fatal(err)
		}
	}
	makeRuntime := func(key, model string, provider *mockprovider.MockProvider) *serveRuntime {
		rt := &serveRuntime{provider: provider, providerKey: key, defaultModel: model, engine: engine.NewEngine(provider, nil), store: store}
		rt.Touch()
		return rt
	}
	previous := makeRuntime("old", "old-model", mockprovider.NewMockProvider("old"))
	candidate := makeRuntime("bad", "bad-model", mockprovider.NewMockProvider("bad").AddError(errors.New("403 forbidden")))
	finalProvider := mockprovider.NewMockProvider("final").AddTextResponse("recovered")
	manager := newServeSessionManager(time.Minute, 100, nil)
	t.Cleanup(manager.Close)
	srv.sessionMgr = manager
	putTestSession(manager, sess.ID, previous)
	srv.runtimeFactory = func(_ context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		if request.Provider == "bad" {
			return candidate, nil
		}
		return makeRuntime("final", "final-model", finalProvider), nil
	}
	input := []core.Message{core.UserText("switch to bad model")}
	exec, err := srv.beginResponseModelSwap(ctx, sess.ID, responseModelSwapPlan{enabled: true, previousProvider: "old", previousModel: "old-model", requestedProvider: "bad", requestedModel: "bad-model"}, input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(exec.markRolledBack)
	run, err := srv.startResponseRun(candidate, true, false, input, core.Request{SessionID: sess.ID, Model: "bad-model"}, sess.ID, startResponseRunOptions{uiSession: true, modelSwap: exec})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.responseRuns.Close() })
	select {
	case <-run.settled:
	case <-time.After(5 * time.Second):
		t.Fatal("failed swap did not settle")
	}
	snapshot := run.snapshot()
	if snapshot["status"] != "failed" {
		t.Fatalf("expected failed swap: %#v", snapshot)
	}
	continuation, _ := snapshot["continuation_response_id"].(string)
	expected := srv.latestDurableResponseIDForSession(ctx, sess.ID)
	if continuation == "" || continuation != expected {
		t.Fatalf("continuation = %q, want durable cursor %q", continuation, expected)
	}
	code, response := doResponsesFirstParty(t, srv, `{"input":"try another model","client_message_id":"retry-swap","previous_response_id":"`+continuation+`","provider":"final","model":"final-model","model_swap":{"mode":"auto","fallback":"handover"}}`)
	if code != http.StatusOK {
		t.Fatalf("retry after failed swap: %d %#v", code, response)
	}
	if text := responseOutputText(t, response); text != "recovered" {
		t.Fatalf("retry output = %q", text)
	}
}
