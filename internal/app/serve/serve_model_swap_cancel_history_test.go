package serve

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
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

// stoppableSwapProvider scripts a swap target: it fails the first calls listed
// in rejects, streams first (text by default) on the next one and waits there
// to be stopped. Later calls use the scripted mock responses.
type stoppableSwapProvider struct {
	*mockprovider.MockProvider
	rejects int32
	first   *core.Event
	finish  bool // complete after first instead of waiting to be stopped
	calls   atomic.Int32
}

func (p *stoppableSwapProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	call := p.calls.Add(1)
	switch {
	case call <= p.rejects:
		return nil, errors.New("400 invalid_request: messages contain incompatible reasoning")
	case call > p.rejects+1:
		return p.MockProvider.Stream(ctx, req)
	}
	return corestream.New(ctx, func(ctx context.Context, send corestream.Sender) error {
		first := core.Event{Type: core.EventTextDelta, Text: "partial target answer"}
		if p.first != nil {
			first = *p.first
		}
		if err := send.Send(first); err != nil {
			return err
		}
		if p.finish {
			return send.Send(core.Event{Type: core.EventUsage, Use: &core.Usage{}})
		}
		<-ctx.Done()
		return ctx.Err()
	}), nil
}

func waitForResponseRunEvent(t *testing.T, run *responseRun, event string) {
	t.Helper()
	sub := run.subscribe(0)
	if sub.ch != nil {
		defer run.unsubscribe(sub.ch)
	}
	for _, replayed := range sub.replay {
		if replayed.Event == event {
			return
		}
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case got, ok := <-sub.ch:
			if !ok {
				t.Fatalf("run ended before %s", event)
			}
			if got.Event == event {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", event)
		}
	}
}

type stoppedSwapFixture struct {
	srv                 *serveServer
	store               *session.SQLiteStore
	sessionID           string
	manager             *serveSessionManager
	source              *mockprovider.MockProvider
	target              *stoppableSwapProvider
	previous, candidate *serveRuntime
	run                 *responseRun
}

// newSwapFixture builds a two-turn old/old-model session about to switch to
// new/new-model. The target rejects its first `rejects` calls and helper
// serves the handover summary on the previous model.
func newSwapFixture(t *testing.T, rejects int32, helper func() core.Provider) *stoppedSwapFixture {
	t.Helper()
	ctx := context.Background()
	srv, store := newServeProjectTestServer(t)
	srv.projectsEnabled = false
	srv.cfgRef = &config.Config{DefaultProvider: "old"}
	srv.shutdownCh = make(chan struct{}) // admit runs with a lifecycle lease, as in production
	sess := &session.Session{ID: session.NewID(), Provider: "old", ProviderKey: "old", Model: "old-model", Mode: session.ModeChat, CompactionSeq: -1}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []core.Message{core.UserText("original question"), core.AssistantText("original answer")} {
		if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
			t.Fatal(err)
		}
	}
	f := &stoppedSwapFixture{srv: srv, store: store, sessionID: sess.ID, source: mockprovider.NewMockProvider("old")}
	f.previous = &serveRuntime{provider: f.source, providerKey: "old", defaultModel: "old-model", engine: engine.NewEngine(f.source, nil), store: store}
	f.previous.Touch()
	f.target = &stoppableSwapProvider{MockProvider: mockprovider.NewMockProvider("new").AddTextResponse("resumed on target"), rejects: rejects}
	f.candidate = &serveRuntime{provider: f.target, providerKey: "new", defaultModel: "new-model", engine: engine.NewEngine(f.target, nil), store: store}
	f.candidate.Touch()
	f.manager = newServeSessionManager(time.Minute, 100, nil)
	t.Cleanup(f.manager.Close)
	srv.sessionMgr = f.manager
	putTestSession(f.manager, sess.ID, f.previous)
	var created atomic.Int32
	srv.runtimeFactory = func(_ context.Context, req serveRuntimeRequest) (*serveRuntime, error) {
		if req.Provider == "old" {
			p := helper()
			return &serveRuntime{provider: p, providerKey: "old", defaultModel: "old-model", engine: engine.NewEngine(p, nil)}, nil
		}
		if created.Add(1) == 1 {
			return f.candidate, nil
		}
		// Later preparation builds its own runtime on the durable identity.
		return &serveRuntime{provider: f.target, providerKey: "new", defaultModel: "new-model", engine: engine.NewEngine(f.target, nil), store: store}, nil
	}
	return f
}

// start begins the swap turn.
func (f *stoppedSwapFixture) start(t *testing.T) {
	t.Helper()
	input := []core.Message{core.UserText("switch and start")}
	input[0].ClientMessageID = "stopped-question"
	exec, err := f.srv.beginResponseModelSwap(context.Background(), f.sessionID, responseModelSwapPlan{enabled: true, fallbackHandover: true, previousProvider: "old", previousModel: "old-model", requestedProvider: "new", requestedModel: "new-model"}, input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(exec.markRolledBack)
	f.run, err = f.srv.startResponseRun(f.candidate, true, false, input, core.Request{SessionID: f.sessionID, Model: "new-model"}, f.sessionID, startResponseRunOptions{uiSession: true, modelSwap: exec})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.srv.responseRuns.Close() })
}

// startStoppableSwap starts a swap whose target is streaming output.
func startStoppableSwap(t *testing.T, rejects int32) *stoppedSwapFixture {
	t.Helper()
	f := newSwapFixture(t, rejects, func() core.Provider { return mockprovider.NewMockProvider("old").AddTextResponse("handover doc") })
	f.start(t)
	waitForResponseRunEvent(t, f.run, "response.output_text.delta")
	return f
}

func (f *stoppedSwapFixture) stop(t *testing.T) {
	t.Helper()
	if !f.run.cancelRun() {
		t.Fatal("cancel not accepted")
	}
	f.waitSettled(t)
}

func (f *stoppedSwapFixture) waitSettled(t *testing.T) {
	t.Helper()
	select {
	case <-f.run.settled:
	case <-time.After(5 * time.Second):
		t.Fatal("stopped swap did not settle")
	}
}

// requireKeptTarget checks the stopped run kept the target with exactly one
// switch marker, and returns the stored transcript.
func (f *stoppedSwapFixture) requireKeptTarget(t *testing.T, strategy string) []session.Message {
	t.Helper()
	if status := f.run.snapshot()["status"]; status != "cancelled" {
		t.Fatalf("run status = %v, want cancelled", status)
	}
	if current, ok := f.manager.Get(f.sessionID); !ok || current != f.candidate {
		t.Fatal("target runtime was not kept")
	}
	persisted, err := f.store.Get(context.Background(), f.sessionID)
	if err != nil || persisted == nil {
		t.Fatalf("load session: %v", err)
	}
	if persisted.ProviderKey != "new" || persisted.Model != "new-model" || persisted.Status != session.StatusInterrupted {
		t.Fatalf("session = %s/%s %s, want new/new-model and the stopped turn interrupted", persisted.ProviderKey, persisted.Model, persisted.Status)
	}
	stored, err := f.store.GetMessages(context.Background(), f.sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	markers := 0
	for _, msg := range stored {
		if marker, ok := core.ParseModelSwapMarker(msg.ToLLMMessage()); ok {
			markers++
			if marker.Status != "succeeded" || marker.Strategy != strategy || marker.ToProvider != "new" || marker.ToModel != "new-model" {
				t.Fatalf("marker = %#v, want a succeeded %s switch to new/new-model", marker, strategy)
			}
		}
	}
	if markers != 1 {
		t.Fatalf("markers = %d, want 1", markers)
	}
	return stored
}

// Stopping the first turn on a new model ends that turn, not the switch. Once
// the target has streamed output the session must stay on it; rolling back
// would silently hand the next turn to the old model.
func TestStoppedModelSwapAfterOutputKeepsTarget(t *testing.T) {
	f := startStoppableSwap(t, 0)
	f.stop(t)
	stored := f.requireKeptTarget(t, "naive")
	var rows []string
	for _, msg := range stored {
		rows = append(rows, string(msg.Role)+":"+msg.TextContent)
	}
	// Same placement as a completed swap: the switch reads before the target's
	// output. The marker goes into the stored rows, so the reply keeps the turn
	// index its turn wrote; a provider-message round trip would reset it to 0.
	if n := len(stored); n < 3 || stored[n-3].TextContent != "switch and start" || stored[n-2].Role != core.RoleEvent ||
		stored[n-1].TextContent != "partial target answer" || stored[n-1].TurnIndex != 1 {
		t.Fatalf("want trigger, marker, then the partial reply at turn 1; rows = %q", rows)
	}

	continuation, _ := f.run.snapshot()["continuation_response_id"].(string)
	if continuation == "" {
		t.Fatal("missing cancellation continuation cursor")
	}
	code, response := doResponsesFirstParty(t, f.srv, `{"input":"follow up","client_message_id":"follow-up","previous_response_id":"`+continuation+`"}`)
	if code != http.StatusOK {
		t.Fatalf("follow-up after stop: %d %#v", code, response)
	}
	if requests := f.source.RecordedRequests(); len(requests) != 0 {
		t.Fatalf("follow-up went to the previous model: %#v", requests)
	}
	requests := f.target.RecordedRequests()
	if len(requests) != 1 || !requestContainsText(requests[0], "partial target answer") || !requestContainsText(requests[0], "follow up") {
		t.Fatalf("target did not continue the stopped turn: %#v", requests)
	}
	for _, msg := range requests[0].Messages {
		if msg.Role == core.RoleEvent {
			t.Fatal("model-swap marker reached the provider")
		}
	}
}

// Reasoning is the target answering too: stopping it mid-thought must not
// hand the session back to the previous model.
func TestStoppedModelSwapWhileReasoningKeepsTarget(t *testing.T) {
	f := newSwapFixture(t, 0, func() core.Provider { return mockprovider.NewMockProvider("old") })
	f.target.first = &core.Event{Type: core.EventReasoningDelta, ReasoningKind: core.ReasoningKindSummary, Text: "weighing the trade-offs"}
	f.start(t)
	waitForResponseRunEvent(t, f.run, "response.reasoning.delta")
	f.stop(t)
	f.requireKeptTarget(t, "naive")
}

// The handover retry is the target's first real turn too, so a stop after it
// answered keeps the switch, reported as cancelled rather than failed.
func TestStoppedHandoverRetryAfterOutputKeepsTarget(t *testing.T) {
	f := startStoppableSwap(t, 1)
	f.stop(t)
	f.requireKeptTarget(t, "handover")
}

// A stop during the handover summary, before the target answered, rolls the
// switch back and is reported as cancelled, not as a failed swap.
func TestStoppedHandoverBeforeTargetAnswerRollsBack(t *testing.T) {
	summarizing := make(chan struct{})
	f := newSwapFixture(t, 1, func() core.Provider {
		return &cancelledSwapProvider{MockProvider: mockprovider.NewMockProvider("old"), started: summarizing}
	})
	f.start(t)
	select {
	case <-summarizing:
	case <-time.After(5 * time.Second):
		t.Fatal("handover summary did not start")
	}
	f.stop(t)
	if status := f.run.snapshot()["status"]; status != "cancelled" {
		t.Fatalf("run status = %v, want cancelled", status)
	}
	if current, ok := f.manager.Get(f.sessionID); !ok || current != f.previous {
		t.Fatal("previous runtime was not restored")
	}
	if persisted, err := f.store.Get(context.Background(), f.sessionID); err != nil || persisted == nil || persisted.ProviderKey != "old" || persisted.Model != "old-model" {
		t.Fatalf("session after stopped handover = %#v, %v; want old/old-model", persisted, err)
	}
}

// A run that lost its lease no longer owns the session: its stop must not
// rewrite the session's runtime identity.
func TestStoppedModelSwapAfterLeaseLossRollsBack(t *testing.T) {
	f := startStoppableSwap(t, 0)
	if f.run.validateLifecycle == nil {
		t.Fatal("fixture failed: run has no lifecycle lease")
	}
	path, err := session.SessionInputStoreIdentity(f.store)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE serve_response_lifecycle SET lease_expires_at=? WHERE response_id=?`, time.Now().Add(-time.Minute).UnixMilli(), f.run.id); err != nil {
		t.Fatal(err)
	}
	// A failed lease checkpoint may already have cancelled the run.
	f.run.cancelRun()
	f.waitSettled(t)

	if current, ok := f.manager.Get(f.sessionID); !ok || current != f.previous {
		t.Fatal("previous runtime was not restored")
	}
	persisted, err := f.store.Get(context.Background(), f.sessionID)
	if err != nil || persisted == nil {
		t.Fatalf("load session: %v", err)
	}
	if persisted.ProviderKey != "old" || persisted.Model != "old-model" {
		t.Fatalf("session = %s/%s after lease loss, want old/old-model", persisted.ProviderKey, persisted.Model)
	}
}

// A completed swap must leave every stored row as it was: the swap start used
// to rewrite the active context from provider messages, and the marker write
// did again, dropping turn indexes, durations, timestamps, compaction-tail
// flags and persistence-only parts from every row after the first mismatch.
func TestCompletedModelSwapKeepsStoredRows(t *testing.T) {
	ctx := context.Background()
	f := newSwapFixture(t, 0, func() core.Provider { return mockprovider.NewMockProvider("old") })
	f.target.finish = true
	summary := *session.NewMessage(f.sessionID, core.UserText("[Context Compaction]\nsummary"), -1)
	summary.TurnIndex = 2
	compacted := []session.Message{summary}
	for _, msg := range []core.Message{core.AssistantText("I've reviewed the context summary."), core.UserText("original question"), core.AssistantText("original answer")} {
		row := *session.NewMessage(f.sessionID, msg, -1)
		row.CompactionTail = true
		compacted = append(compacted, row)
	}
	if err := f.store.CompactMessages(ctx, f.sessionID, compacted); err != nil {
		t.Fatal(err)
	}
	note := session.NewPathNoteMessage(f.sessionID, "explored the branch", core.PathNoteProvenance{SourceSessionID: "branch"}, -1)
	note.TurnIndex = 3
	question := session.NewMessage(f.sessionID, core.UserText("later question"), -1)
	question.TurnIndex = 3
	answer := session.NewMessage(f.sessionID, core.AssistantText("later answer"), -1)
	answer.TurnIndex, answer.DurationMs = 3, 1234
	for _, row := range []*session.Message{note, question, answer} {
		if err := f.store.AddMessage(ctx, f.sessionID, row); err != nil {
			t.Fatal(err)
		}
	}
	before, err := f.store.GetMessages(ctx, f.sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	sessBefore, err := f.store.Get(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}

	f.start(t)
	f.waitSettled(t)
	if status := f.run.snapshot()["status"]; status != "completed" {
		t.Fatalf("run status = %v, want completed", status)
	}

	after, err := f.store.GetMessages(ctx, f.sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+3 {
		t.Fatalf("rows = %d, want the %d prior rows plus trigger, marker and reply", len(after), len(before))
	}
	for i, want := range before {
		got := after[i]
		if got.ID != want.ID || got.TurnIndex != want.TurnIndex || got.DurationMs != want.DurationMs || got.CompactionTail != want.CompactionTail ||
			!got.CreatedAt.Equal(want.CreatedAt) || len(got.Parts) != len(want.Parts) {
			t.Fatalf("row %d changed across the swap:\n got %+v\nwant %+v", i, got, want)
		}
	}
	tail := after[len(before):]
	if tail[0].TextContent != "switch and start" || tail[1].Role != core.RoleEvent || tail[2].TextContent != "partial target answer" {
		t.Fatalf("want trigger, marker, reply after the prior rows; got %+v", tail)
	}
	sessAfter, err := f.store.Get(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sessAfter.UserTurns != sessBefore.UserTurns+1 || sessAfter.CompactionSeq != sessBefore.CompactionSeq {
		t.Fatalf("user turns/boundary %d/%d -> %d/%d, want one new turn and the same boundary",
			sessBefore.UserTurns, sessBefore.CompactionSeq, sessAfter.UserTurns, sessAfter.CompactionSeq)
	}
}
