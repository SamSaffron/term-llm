package serve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestGoalCancelledBeforeRuntimeAdmissionWithoutStopHook(t *testing.T) {
	dir, err := canonicalRootLeasePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Hold mutation admission so cancellation happens before runOnce owns the
	// runtime and installs its normal child-sweep finalizer.
	release, blocked := processRootCheckoutLeases.lease(dir).tryAcquireWrite(false)
	if blocked != rootCheckoutMutationAvailable {
		t.Fatal("cannot hold checkout mutation")
	}
	defer release()
	run := newResponseRun("lost-lease", "goal-parent", "", "mock", time.Now().Unix(), func() {})
	ctx, cancel := context.WithCancel(withResponseRunContext(t.Context(), run))
	cancel()
	rt := &serveRuntime{}
	_, err = rt.runWithGoal(ctx, false, false, nil, core.Request{SessionID: "goal-parent", WorkingDir: dir}, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission = %v", err)
	}
}

func TestFencedTerminalStopPreservesEarlierCompletion(t *testing.T) {
	_, store, rt := newAgentWakeFixture(t, mockprovider.NewMockProvider("debug"))
	for _, id := range []string{"earlier", "current"} {
		row := session.AgentRun{ID: id, ParentSessionID: "wake-parent", ParentResponseID: id + "-turn", Status: "completed", UpdatedAt: time.Now()}
		if err := store.PutAgentRun(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	ctx := session.WithResponseRunFence(t.Context(), session.ResponseRunFence{ResponseID: "current-turn", OwnerInstanceID: "tui-owner", FencingToken: 1})
	await := rt.signalStoppedChildren(ctx, "wake-parent")
	if await == nil {
		t.Fatal("human stop did not provide child cleanup")
	}
	await(t.Context())
	for _, id := range []string{"earlier", "current"} {
		row, err := store.GetAgentRun(t.Context(), id)
		if err != nil || row.WakeSuppressed != (id == "current") {
			t.Fatalf("%s suppression = %t, error = %v", id, row.WakeSuppressed, err)
		}
	}
}

func TestHostedChildFollowupAdmissionFailureDoesNotUndoStartedRun(t *testing.T) {
	provider := mockprovider.NewMockProvider("debug").AddTextResponse("own turn executed")
	srv, _, rt := newAgentWakeFixture(t, provider)
	env := &cmdRunEnvironment{runtime: rt, req: runpkg.Request{SessionID: "wake-parent", IsSubagent: true}}
	handle := &childRunHandle{srv: srv, childSessionID: "wake-parent"}
	turn := handle.runtimeTurn(env)
	request := core.Request{SessionID: "wake-parent", Model: "fast"}
	if _, err := turn(t.Context(), true, []core.Message{core.UserText("work")}, request, nil); err != nil {
		t.Fatal(err)
	}
	if provider.CurrentTurn() != 1 {
		t.Fatal("child's own turn did not execute")
	}
	// Admission contention after a completed own turn must not tell finish()
	// that the entire resumed execution never started.
	srv.responseRuns.setActiveRun("wake-parent", "competing-run")
	defer srv.responseRuns.clearActiveRun("wake-parent", "competing-run")
	for _, own := range []bool{false, true} {
		_, err := turn(t.Context(), own, nil, request, nil)
		if !errors.Is(err, errServeSessionBusy) {
			t.Fatalf("own=%t: admission error = %v", own, err)
		}
		var admission *tools.AgentRunAdmissionError
		if errors.As(err, &admission) != own {
			t.Fatalf("own=%t: incorrectly classified never-started execution: %v", own, err)
		}
	}
}

func TestDeletedAgentWakeParentDoesNotRetry(t *testing.T) {
	provider := mockprovider.NewMockProvider("debug")
	srv, store, _ := newAgentWakeFixture(t, provider)
	putWakeRun(t, store, "completed")
	if err := store.Delete(t.Context(), "wake-parent"); err != nil {
		t.Fatal(err)
	}
	if !srv.deliverAgentWake("wake-parent") {
		t.Fatal("deleted parent treated as a transient delivery failure")
	}
	if provider.CurrentTurn() != 0 {
		t.Fatal("deleted parent was reawakened")
	}
}

func TestHostedChildInitialAdmissionFailureDoesNotRunLifecycleFollowup(t *testing.T) {
	_, store, rt := newAgentWakeFixture(t, mockprovider.NewMockProvider("debug"))
	row := putWakeRun(t, store, "completed")
	env := &cmdRunEnvironment{runtime: rt, store: store, req: runpkg.Request{SessionID: row.ParentSessionID, IsSubagent: true}}
	calls := 0
	_, err := env.runChildLifecycleTurns(t.Context(), eventSinkFunc(nil), func(_ context.Context, own bool, _ []core.Message, _ core.Request, _ func(core.Event) error) (serveRunResult, error) {
		calls++
		if own {
			return serveRunResult{}, &tools.AgentRunAdmissionError{Err: errServeSessionBusy}
		}
		return serveRunResult{}, nil
	})
	var admission *tools.AgentRunAdmissionError
	if !errors.As(err, &admission) || calls != 1 {
		t.Fatalf("initial refusal performed follow-up work: calls=%d err=%v", calls, err)
	}
	pending, err := store.GetAgentRun(t.Context(), row.ID)
	if err != nil || !pending.NotifiedAt.IsZero() {
		t.Fatalf("unstarted run consumed a lifecycle event: %+v err=%v", pending, err)
	}
}
