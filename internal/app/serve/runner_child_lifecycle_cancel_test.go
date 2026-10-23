package serve

import (
	"context"
	"errors"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

func TestChildLifecyclePreservesCancellationAfterInitialTurn(t *testing.T) {
	_, store, rt := newAgentWakeFixture(t, mockprovider.NewMockProvider("debug"))
	row := putWakeRun(t, store, "completed")
	env := &cmdRunEnvironment{runtime: rt, store: store, req: runpkg.Request{SessionID: row.ParentSessionID, IsSubagent: true}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	_, err := env.runChildLifecycleTurns(ctx, eventSinkFunc(nil), func(_ context.Context, own bool, _ []core.Message, _ core.Request, _ func(core.Event) error) (serveRunResult, error) {
		calls++
		if !own {
			t.Fatal("lifecycle turn ran after cancellation")
		}
		// Cancellation can arrive as the initial turn finishes successfully,
		// before the child has entered its barrier wait.
		cancel()
		return serveRunResult{}, nil
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancelled child outcome: calls=%d err=%v", calls, err)
	}
	pending, err := store.GetAgentRun(t.Context(), row.ID)
	if err != nil || !pending.NotifiedAt.IsZero() {
		t.Fatalf("cancelled run consumed a lifecycle event: %+v err=%v", pending, err)
	}
}
