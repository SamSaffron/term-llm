package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type leasedTestClientRunner struct {
	lease *ClientToolLease
	run   func(context.Context, ToolCall) (ToolOutput, error)
}

func (r leasedTestClientRunner) ClientToolLease() *ClientToolLease { return r.lease }
func (r leasedTestClientRunner) OwnsClientTool(name string) bool {
	_, ok := r.lease.Spec(name)
	return ok
}
func (r leasedTestClientRunner) RunClientTool(ctx context.Context, call ToolCall) (ToolOutput, error) {
	return r.run(ctx, call)
}

func newTestClientLease(t *testing.T) (*ClientToolLease, []ToolSpec) {
	t.Helper()
	specs := []ToolSpec{{Name: "phone", Description: "original phone", Schema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "string"}}}
	lease, err := NewClientToolLease(ClientToolBinding{ResponseID: "resp_1", SessionID: "session_1", UserID: "user_1", ClientID: "device_1", Generation: "page_1"}, specs)
	if err != nil {
		t.Fatal(err)
	}
	return lease, specs
}

func assertClientFailure(t *testing.T, err error, code, outcome string) {
	t.Helper()
	var clientErr *ClientToolError
	if !errors.As(err, &clientErr) || clientErr.Code != code || clientErr.Outcome != outcome {
		t.Fatalf("error = %v; want %s/%s", err, code, outcome)
	}
}

func TestClientLeaseScopeAndFullSnapshot(t *testing.T) {
	lease, specs := newTestClientLease(t)
	runner := leasedTestClientRunner{lease: lease}
	ambient := ContextWithClientToolRunner(context.Background(), runner)
	if _, got, _ := CaptureClientTools(ambient); got != nil {
		t.Fatal("ambient context granted cell capability")
	}
	ctx := ScopeClientToolRunner(ambient, specs)
	specs[0].Schema["type"] = "mutated"
	_, got, captured := CaptureClientTools(ctx)
	if got != lease || len(captured) != 1 || captured[0].Schema["type"] != "object" || captured[0].OutputSchema["type"] != "string" || captured[0].Description != "original phone" {
		t.Fatalf("incomplete or mutable schema capture: %#v", captured)
	}
	captured[0].Schema["type"] = "other mutation"
	frozen, _ := lease.Spec("phone")
	if frozen.Schema["type"] != "object" {
		t.Fatal("captured schema mutated lease")
	}
	// Even a child offering identical names cannot inherit its parent's lease.
	child := ScopeClientToolRunner(ctx, []ToolSpec{frozen})
	if _, got, _ := CaptureClientTools(child); got != nil || ClientTool(child, "phone") != nil {
		t.Fatal("child inherited parent's runner")
	}
	changed := ScopeClientToolRunner(ambient, specs)
	if _, _, offered := CaptureClientTools(changed); len(offered) != 0 {
		t.Fatal("changed request schema matched original lease")
	}
	lease.Revoke("response ended")
	if _, got, _ := CaptureClientTools(ctx); got != nil {
		t.Fatal("expired lease admitted a new cell")
	}
}

func TestClientLeaseForegroundResultAndNoReplay(t *testing.T) {
	lease, _ := newTestClientLease(t)
	var calls atomic.Int32
	runner := leasedTestClientRunner{lease: lease, run: func(ctx context.Context, call ToolCall) (ToolOutput, error) {
		if err := BeginClientToolDispatch(ctx); err != nil {
			return ToolOutput{}, err
		}
		calls.Add(1)
		return TextOutput("pong:" + call.ID), nil
	}}
	call := ToolCall{ID: "call_1", Name: "phone"}
	out, err := lease.Dispatch(context.Background(), runner, call)
	if err != nil || out.Content != "pong:call_1" {
		t.Fatalf("foreground result: %+v, %v", out, err)
	}
	_, err = lease.Dispatch(context.Background(), runner, call)
	assertClientFailure(t, err, "client_call_duplicate", "not_dispatched")
	lease.Revoke("response completed")
	_, err = lease.Dispatch(context.Background(), runner, ToolCall{ID: "call_2", Name: "phone"})
	assertClientFailure(t, err, "client_binding_expired", "not_dispatched")
	if calls.Load() != 1 {
		t.Fatal("expired/duplicate request replayed")
	}
}

func TestClientLeaseExpiryAndCancellationBoundaries(t *testing.T) {
	for _, published := range []bool{false, true} {
		for _, cancelled := range []bool{false, true} {
			t.Run(map[bool]string{false: "queued", true: "published"}[published]+map[bool]string{false: "/expiry", true: "/cancel"}[cancelled], func(t *testing.T) {
				lease, _ := newTestClientLease(t)
				ready, release, settled := make(chan struct{}), make(chan struct{}), make(chan struct{})
				runner := leasedTestClientRunner{lease: lease, run: func(ctx context.Context, _ ToolCall) (ToolOutput, error) {
					defer close(settled)
					if published {
						if err := BeginClientToolDispatch(ctx); err != nil {
							return ToolOutput{}, err
						}
					}
					close(ready)
					<-release // simulate a late device reply that ignores cancellation
					return TextOutput("late device result"), nil
				}}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { _, err := lease.Dispatch(ctx, runner, ToolCall{ID: "race", Name: "phone"}); done <- err }()
				<-ready
				code := "client_binding_expired"
				if cancelled {
					cancel()
					code = "cancelled"
				} else {
					lease.Revoke("owner ended")
				}
				outcome := "not_dispatched"
				if published {
					outcome = "outcome_unknown"
				}
				select {
				case err := <-done:
					assertClientFailure(t, err, code, outcome)
				case <-time.After(time.Second):
					t.Fatal("revocation did not release waiter")
				}
				close(release)
				<-settled
			})
		}
	}
}

func TestClientLeaseTimeoutIsNotLeaseExpiry(t *testing.T) {
	lease, _ := newTestClientLease(t)
	runner := leasedTestClientRunner{lease: lease, run: func(ctx context.Context, _ ToolCall) (ToolOutput, error) {
		if err := BeginClientToolDispatch(ctx); err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{}, context.DeadlineExceeded // transport's own device timeout
	}}
	_, err := lease.Dispatch(context.Background(), runner, ToolCall{ID: "timeout", Name: "phone"})
	assertClientFailure(t, err, "client_tool_error", "outcome_unknown")
	if !errors.Is(err, context.DeadlineExceeded) || lease.Check() != nil {
		t.Fatal("device timeout expired foreground lease")
	}
	// An observation deadline never touches the lease either.
	observing, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = lease.Dispatch(observing, runner, ToolCall{ID: "pre_cancel", Name: "phone"})
	assertClientFailure(t, err, "cancelled", "not_dispatched")
	if lease.Check() != nil {
		t.Fatal("observation cancellation expired lease")
	}
}

func TestClientLeaseDispatchRevokeRace(t *testing.T) {
	for range 100 {
		lease, _ := newTestClientLease(t)
		var published atomic.Bool
		runner := leasedTestClientRunner{lease: lease, run: func(ctx context.Context, _ ToolCall) (ToolOutput, error) {
			if err := BeginClientToolDispatch(ctx); err != nil {
				return ToolOutput{}, err
			}
			published.Store(true)
			return TextOutput("done"), nil
		}}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); lease.Revoke("race") }()
		go func() {
			defer wg.Done()
			_, err := lease.Dispatch(context.Background(), runner, ToolCall{ID: "race", Name: "phone"})
			if err != nil {
				var failure *ClientToolError
				if !errors.As(err, &failure) {
					t.Errorf("unclassified error: %v", err)
				}
			}
		}()
		wg.Wait()
		if err := BeginClientToolDispatch(context.WithValue(context.Background(), clientToolDispatchKey{}, &clientToolDispatch{lease: lease})); err == nil {
			t.Fatal("expired binding admitted publication")
		}
	}
}
