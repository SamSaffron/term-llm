package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSyncToolSupervisorRunsParallelAndSettlesInDispatchOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSyncToolSupervisor(ctx, true)
	started := make(chan string, 2)
	release := make(chan struct{})
	responded := make(chan string, 2)

	for _, id := range []string{"first", "second"} {
		id := id
		s.dispatch(ToolCall{ID: id, Name: "spawn_agent"}, func(context.Context) toolCallOutcome {
			started <- id
			<-release
			return toolCallOutcome{call: ToolCall{ID: id, Name: "spawn_agent"}, output: TextOutput(id)}
		}, func(outcome toolCallOutcome) {
			responded <- outcome.call.ID
		})
	}

	seen := map[string]bool{}
	for range 2 {
		select {
		case id := <-started:
			seen[id] = true
		case <-time.After(time.Second):
			t.Fatal("parallel supervisor did not start both calls")
		}
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("started calls = %v", seen)
	}
	close(release)
	outcomes := s.settle(ctx)
	if len(outcomes) != 2 || outcomes[0].call.ID != "first" || outcomes[1].call.ID != "second" {
		t.Fatalf("settled outcomes = %#v", outcomes)
	}
	for range 2 {
		select {
		case <-responded:
		case <-time.After(time.Second):
			t.Fatal("missing bridge response")
		}
	}
}

func TestSyncToolSupervisorSettlementIncludesBridgeDelivery(t *testing.T) {
	s := newSyncToolSupervisor(context.Background(), true)
	response := make(chan toolCallOutcome)
	s.dispatch(ToolCall{ID: "delivery", Name: "probe"}, func(context.Context) toolCallOutcome {
		return toolCallOutcome{call: ToolCall{ID: "delivery", Name: "probe"}, output: TextOutput("done")}
	}, func(outcome toolCallOutcome) { response <- outcome })

	settled := make(chan []toolCallOutcome, 1)
	go func() { settled <- s.settle(context.Background()) }()
	select {
	case <-settled:
		t.Fatal("settlement completed before bridge accepted terminal outcome")
	case <-time.After(50 * time.Millisecond):
	}
	if got := <-response; got.output.Content != "done" {
		t.Fatalf("bridge outcome = %#v", got)
	}
	select {
	case outcomes := <-settled:
		if len(outcomes) != 1 || outcomes[0].output.Content != "done" {
			t.Fatalf("settled outcomes = %#v", outcomes)
		}
	case <-time.After(time.Second):
		t.Fatal("settlement did not complete after bridge delivery")
	}
}

func TestSyncToolSupervisorSerialModePreservesFIFOAndEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSyncToolSupervisor(ctx, false)
	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	secondStarted := make(chan struct{})
	var once sync.Once

	s.dispatch(ToolCall{ID: "first", Name: "probe"}, func(context.Context) toolCallOutcome {
		close(firstStarted)
		<-firstRelease
		return toolCallOutcome{call: ToolCall{ID: "first", Name: "probe"}, output: TextOutput("first result")}
	}, func(toolCallOutcome) {})
	s.dispatch(ToolCall{ID: "second", Name: "probe"}, func(context.Context) toolCallOutcome {
		calls, outcomes := s.evidenceFor("second")
		if len(calls) != 2 || calls[0].ID != "first" || calls[1].ID != "second" {
			t.Errorf("evidence calls = %#v", calls)
		}
		if len(outcomes) != 1 || outcomes[0].output.Content != "first result" {
			t.Errorf("evidence outcomes = %#v", outcomes)
		}
		once.Do(func() { close(secondStarted) })
		return toolCallOutcome{call: ToolCall{ID: "second", Name: "probe"}, output: TextOutput("second result")}
	}, func(toolCallOutcome) {})

	<-firstStarted
	select {
	case <-secondStarted:
		t.Fatal("second call started before first completed")
	default:
	}
	close(firstRelease)
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second call did not start")
	}
	outcomes := s.settle(ctx)
	if len(outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(outcomes))
	}
}

func TestSyncToolSupervisorAbortPublishesSameOutcomeAndSuppressesLateResult(t *testing.T) {
	s := newSyncToolSupervisor(context.Background(), true)
	started := make(chan struct{})
	release := make(chan struct{})
	responded := make(chan toolCallOutcome, 2)
	s.dispatch(ToolCall{ID: "blocked", Name: "probe"}, func(context.Context) toolCallOutcome {
		close(started)
		<-release
		return toolCallOutcome{call: ToolCall{ID: "blocked", Name: "probe"}, output: TextOutput("late")}
	}, func(outcome toolCallOutcome) { responded <- outcome })
	<-started

	cause := errors.New("stream failed")
	outcomes := s.abort(cause)
	if len(outcomes) != 1 || !errors.Is(outcomes[0].err, cause) {
		t.Fatalf("aborted outcomes = %#v", outcomes)
	}
	first := <-responded
	if !errors.Is(first.err, cause) {
		t.Fatalf("bridge outcome = %#v, want stream failure", first)
	}
	close(release)
	select {
	case duplicate := <-responded:
		t.Fatalf("late worker published duplicate outcome: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSyncToolSupervisorContainsRunPanic(t *testing.T) {
	s := newSyncToolSupervisor(context.Background(), false)
	responded := make(chan toolCallOutcome, 1)
	s.dispatch(ToolCall{ID: "panic", Name: "probe"}, func(context.Context) toolCallOutcome {
		panic("boom")
	}, func(outcome toolCallOutcome) { responded <- outcome })

	outcomes := s.settle(context.Background())
	if len(outcomes) != 1 || outcomes[0].err == nil || !strings.Contains(outcomes[0].err.Error(), "boom") {
		t.Fatalf("panic outcomes = %#v", outcomes)
	}
	if got := <-responded; got.err == nil || !strings.Contains(got.err.Error(), "boom") {
		t.Fatalf("bridge panic outcome = %#v", got)
	}
}

func TestSyncToolSupervisorDispatchDoesNotBlockWhenWorkersAreBusy(t *testing.T) {
	s := newSyncToolSupervisor(context.Background(), false)
	started := make(chan struct{})
	release := make(chan struct{})
	s.dispatch(ToolCall{ID: "first", Name: "probe"}, func(context.Context) toolCallOutcome {
		close(started)
		<-release
		return toolCallOutcome{call: ToolCall{ID: "first", Name: "probe"}}
	}, func(toolCallOutcome) {})
	<-started

	dispatched := make(chan struct{})
	go func() {
		for i := range 200 {
			id := fmt.Sprintf("queued-%d", i)
			s.dispatch(ToolCall{ID: id, Name: "probe"}, func(context.Context) toolCallOutcome {
				return toolCallOutcome{call: ToolCall{ID: id, Name: "probe"}}
			}, func(toolCallOutcome) {})
		}
		close(dispatched)
	}()
	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("dispatch blocked behind busy worker")
	}
	s.abort(errors.New("test complete"))
	close(release)
}

func TestSyncToolSupervisorCancellationDoesNotWaitForNonCooperativeCall(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	s := newSyncToolSupervisor(parent, true)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s.dispatch(ToolCall{ID: "blocked", Name: "probe"}, func(context.Context) toolCallOutcome {
		close(started)
		<-release
		return toolCallOutcome{call: ToolCall{ID: "blocked", Name: "probe"}, output: TextOutput("late")}
	}, func(toolCallOutcome) {})
	<-started
	cancel()

	startedAt := time.Now()
	outcomes := s.settle(parent)
	if elapsed := time.Since(startedAt); elapsed > 200*time.Millisecond {
		t.Fatalf("cancelled settle took %v", elapsed)
	}
	if len(outcomes) != 1 || outcomes[0].err == nil {
		t.Fatalf("cancelled outcomes = %#v", outcomes)
	}
}
