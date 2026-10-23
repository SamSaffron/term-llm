package serve

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

type cancelledSwapProvider struct {
	*mockprovider.MockProvider
	started chan struct{}
}

func (p *cancelledSwapProvider) Stream(ctx context.Context, _ core.Request) (core.Stream, error) {
	close(p.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCancelledModelSwapReloadsRetainedRuntime(t *testing.T) {
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
	source := mockprovider.NewMockProvider("old").AddTextResponse("resumed")
	previous := &serveRuntime{provider: source, providerKey: "old", defaultModel: "old-model", engine: engine.NewEngine(source, nil), store: store}
	previous.Touch()
	target := &cancelledSwapProvider{MockProvider: mockprovider.NewMockProvider("new"), started: make(chan struct{})}
	candidate := &serveRuntime{provider: target, providerKey: "new", defaultModel: "new-model", engine: engine.NewEngine(target, nil), store: store}
	candidate.Touch()
	manager := newServeSessionManager(time.Minute, 100, nil)
	t.Cleanup(manager.Close)
	srv.sessionMgr = manager
	putTestSession(manager, sess.ID, previous)
	srv.runtimeFactory = func(context.Context, serveRuntimeRequest) (*serveRuntime, error) { return candidate, nil }
	input := []core.Message{core.UserText("cancelled swap question")}
	input[0].ClientMessageID = "cancelled-question"
	exec, err := srv.beginResponseModelSwap(ctx, sess.ID, responseModelSwapPlan{enabled: true, previousProvider: "old", previousModel: "old-model", requestedProvider: "new", requestedModel: "new-model"}, input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(exec.markRolledBack)
	run, err := srv.startResponseRun(candidate, true, false, input, core.Request{SessionID: sess.ID, Model: "new-model"}, sess.ID, startResponseRunOptions{uiSession: true, modelSwap: exec})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.responseRuns.Close() })
	select {
	case <-target.started:
	case <-time.After(5 * time.Second):
		t.Fatal("target did not start")
	}
	if !run.cancelRun() {
		t.Fatal("cancel not accepted")
	}
	select {
	case <-run.settled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled swap did not settle")
	}
	snapshot := run.snapshot()
	if snapshot["status"] != "cancelled" {
		t.Fatalf("status = %v", snapshot["status"])
	}
	if current, ok := manager.Get(sess.ID); !ok || current != previous {
		t.Fatal("previous runtime was not restored")
	}
	continuation, _ := snapshot["continuation_response_id"].(string)
	if continuation == "" {
		t.Fatal("missing cancellation continuation cursor")
	}
	code, response := doResponsesFirstParty(t, srv, `{"input":"follow up","client_message_id":"follow-up","previous_response_id":"`+continuation+`"}`)
	if code != http.StatusOK {
		t.Fatalf("follow-up after cancellation: %d %#v", code, response)
	}
	requests := source.RecordedRequests()
	if len(requests) != 1 || !requestContainsText(requests[0], "cancelled swap question") || !requestContainsText(requests[0], "follow up") {
		t.Fatalf("retained runtime lost durable cancelled turn: %#v", requests)
	}
}
