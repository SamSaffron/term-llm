package serve

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestFailedRunPreservesDurableToolRounds(t *testing.T) {
	for _, mode := range []string{"ordinary", "failed effort swap", "after failed effort swap"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			srv, store := newServeProjectTestServer(t)
			srv.projectsEnabled = false
			srv.cfgRef = &config.Config{DefaultProvider: "flashnext"}
			sess := &session.Session{ID: session.NewID(), Provider: "flashnext", ProviderKey: "flashnext", Model: "flashnext", ReasoningEffort: "medium", Mode: session.ModeChat, CompactionSeq: -1}
			if err := store.Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			for _, msg := range []core.Message{core.UserText("old question"), core.AssistantText("old answer")} {
				if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
					t.Fatal(err)
				}
			}
			providerErr := errors.New("provider stream failed after durable tool rounds")
			provider := mockprovider.NewMockProvider("flashnext")
			tool := &serveRuntimeTestTool{}
			for i := range 3 {
				provider.AddToolCall(fmt.Sprintf("call-%d", i), tool.Spec().Name, map[string]any{})
			}
			provider.AddError(providerErr)
			makeRuntime := func(provider *mockprovider.MockProvider) *serveRuntime {
				registry := engine.NewToolRegistry()
				registry.Register(tool)
				rt := &serveRuntime{provider: provider, providerKey: "flashnext", defaultModel: "flashnext", engine: engine.NewEngine(provider, registry), store: store}
				rt.Touch()
				return rt
			}
			previous := makeRuntime(provider)
			candidate := makeRuntime(provider)
			manager := newServeSessionManager(time.Minute, 10, nil)
			t.Cleanup(manager.Close)
			srv.sessionMgr = manager
			putTestSession(manager, sess.ID, previous)
			srv.runtimeFactory = func(context.Context, serveRuntimeRequest) (*serveRuntime, error) { return candidate, nil }
			plan := responseModelSwapPlan{enabled: true, previousProvider: "flashnext", previousModel: "flashnext", previousEffort: "medium", requestedProvider: "flashnext", requestedModel: "flashnext", requestedEffort: "xhigh"}
			if mode == "after failed effort swap" {
				candidate = makeRuntime(mockprovider.NewMockProvider("flashnext").AddError(errors.New("effort probe failed")))
				input := []core.Message{core.UserText("failed switch input")}
				exec, err := srv.beginResponseModelSwap(ctx, sess.ID, plan, input)
				if err != nil {
					t.Fatal(err)
				}
				_, _, err = srv.runResponseWithModelSwapFallback(ctx, candidate, true, false, input, core.Request{SessionID: sess.ID}, sess.ID, exec)
				if err == nil {
					t.Fatal("expected probe failure")
				}
				stored, err := store.GetMessages(ctx, sess.ID, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				if !sessionMessagesContainText(stored, "failed switch input") {
					t.Fatal("rollback removed the durably committed failed-switch input")
				}
			}
			var durable []session.Message
			capture := func(context.Context, int, []core.Message, core.TurnMetrics) error {
				var err error
				durable, err = store.GetMessages(ctx, sess.ID, 0, 0)
				return err
			}
			input := []core.Message{core.UserText("run three tools then fail")}
			req := core.Request{SessionID: sess.ID, Model: "flashnext", Tools: []core.ToolSpec{tool.Spec()}, MaxTurns: 10}
			var err error
			if mode == "failed effort swap" {
				exec, beginErr := srv.beginResponseModelSwap(ctx, sess.ID, plan, input)
				if beginErr != nil {
					t.Fatal(beginErr)
				}
				candidate.turnCompletedCB = capture
				_, _, err = srv.runResponseWithModelSwapFallback(ctx, candidate, true, false, input, req, sess.ID, exec)
			} else {
				previous.turnCompletedCB = capture
				_, err = previous.Run(ctx, true, false, input, req)
			}
			if !errors.Is(err, providerErr) {
				t.Fatalf("Run error = %v, want %v", err, providerErr)
			}
			if len(durable) < 9 {
				t.Fatalf("expected input and three durable tool rounds, got %d rows", len(durable))
			}
			stored, err := store.GetMessages(ctx, sess.ID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			assertDurableMessagePrefix(t, durable, stored)
			if mode == "failed effort swap" {
				if !previous.ensurePersistedSession(ctx, sess.ID, nil) {
					t.Fatal("could not hydrate restored runtime")
				}
				if len(previous.history) != len(stored) {
					t.Fatalf("restored runtime has %d messages, durable store has %d", len(previous.history), len(stored))
				}
			}
		})
	}
}

func assertDurableMessagePrefix(t *testing.T, before, after []session.Message) {
	t.Helper()
	if len(after) < len(before) {
		t.Fatalf("failure removed durable rows: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID || before[i].Sequence != after[i].Sequence || before[i].Role != after[i].Role || !reflect.DeepEqual(before[i].Parts, after[i].Parts) {
			t.Fatalf("failure rewrote durable row %d: before=%+v after=%+v", i, before[i], after[i])
		}
	}
}

func TestFailureReconciliationDoesNotReplaceDurableTranscript(t *testing.T) {
	ctx := context.Background()
	_, store := newServeProjectTestServer(t)
	sess := &session.Session{ID: session.NewID(), Mode: session.ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	base := []core.Message{core.UserText("old question"), core.AssistantText("old answer")}
	input := []core.Message{core.UserText("durable new input")}
	produced := []core.Message{core.AssistantText("durable output")}
	for _, msg := range append(append(copyLLMMessageSlice(base), input...), produced...) {
		if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
			t.Fatal(err)
		}
	}
	durable, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	rt := &serveRuntime{store: store}
	// A failed incremental write has disabled append-only mode. Even if the
	// in-memory base is divergent, finalization must not replace durable rows.
	p := newServeRunPersistence(rt, sess.ID, 1, serveRunSpec{stateful: true, persisted: true}, base[:1], input, false, "")
	p.produced = produced
	p.initialPersisted = true
	p.lastAppendedIdx = len(produced)
	p.reconcileFailure(ctx, ctx, errors.New("provider failed"), "", func() {})
	stored, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertDurableMessagePrefix(t, durable, stored)
}
