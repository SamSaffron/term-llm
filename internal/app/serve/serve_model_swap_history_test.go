package serve

import (
	"context"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestConsecutiveModelSwapsPreserveTranscriptMarkers(t *testing.T) {
	ctx := context.Background()
	srv, store := newServeProjectTestServer(t)
	sess := &session.Session{ID: session.NewID(), Provider: "mock", ProviderKey: "mock", Model: "original", Mode: session.ModeChat, CompactionSeq: -1}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	manager := newServeSessionManager(time.Minute, 10, nil)
	t.Cleanup(manager.Close)
	srv.sessionMgr = manager
	newRuntime := func(model string) (*serveRuntime, *mockprovider.MockProvider) {
		provider := mockprovider.NewMockProvider("mock").AddTextResponse("ready")
		rt := &serveRuntime{provider: provider, engine: engine.NewEngine(provider, nil), store: store, providerKey: "mock", defaultModel: model}
		rt.Touch()
		return rt, provider
	}
	previous, _ := newRuntime("original")
	putTestSession(manager, sess.ID, previous)

	from := "original"
	for i, target := range []string{"other", "original"} {
		candidate, provider := newRuntime(target)
		srv.runtimeFactory = func(context.Context, serveRuntimeRequest) (*serveRuntime, error) { return candidate, nil }
		plan := responseModelSwapPlan{enabled: true, previousProvider: "mock", previousModel: from, requestedProvider: "mock", requestedModel: target}
		input := []core.Message{core.UserText("continue")}
		exec, err := srv.beginResponseModelSwap(ctx, sess.ID, plan, input)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(exec.markRolledBack)
		if _, _, err := srv.runResponseWithModelSwapFallback(ctx, candidate, true, false, input, core.Request{SessionID: sess.ID, Model: target}, sess.ID, exec); err != nil {
			t.Fatal(err)
		}

		stored, err := store.GetMessages(ctx, sess.ID, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		var markers []core.ModelSwapMarker
		for index, msg := range stored {
			if marker, ok := core.ParseModelSwapMarker(msg.ToLLMMessage()); ok {
				markers = append(markers, marker)
				if index == 0 || stored[index-1].Role != core.RoleUser || index+1 == len(stored) || stored[index+1].Role != core.RoleAssistant {
					t.Fatalf("marker is not between its triggering user message and reply: index %d", index)
				}
			}
		}
		if len(markers) != i+1 {
			t.Fatalf("after swap %d: got %d markers, want %d", i+1, len(markers), i+1)
		}
		if markers[0].FromModel != "original" || markers[0].ToModel != "other" {
			t.Fatalf("lost first transition: %#v", markers)
		}
		if markers[i].FromModel != from || markers[i].ToModel != target {
			t.Fatalf("incorrect latest transition: %#v", markers)
		}
		for _, req := range provider.Requests {
			for _, msg := range req.Messages {
				if msg.Role == core.RoleEvent {
					t.Fatal("transcript event leaked to provider")
				}
			}
		}
		from = target
	}
}
