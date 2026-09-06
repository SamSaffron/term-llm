package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPExecOwnerCancelsAndJoinsMutation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := &processExecOwner{ctx: ctx, mode: "test", grace: 20 * time.Millisecond, cancels: make(map[uint64]context.CancelFunc)}
	entered, returned, executed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	owner.exec = func(string, []string, []string) error {
		if calls.Add(1) != 1 {
			t.Error("duplicate replacement")
		}
		select {
		case <-returned:
		default:
			t.Error("replaced before handler settled")
		}
		close(executed)
		return errors.New("fixture exec failure")
	}
	handler := owner.handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(returned) }))
	go handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mutate", nil))
	<-entered
	owner.request()
	owner.request()
	reject := httptest.NewRecorder()
	handler.ServeHTTP(reject, httptest.NewRequest(http.MethodPost, "/another", nil))
	if reject.Code != http.StatusServiceUnavailable {
		t.Fatal("mutation admitted while draining")
	}
	select {
	case <-executed:
	case <-time.After(time.Second):
		t.Fatal("owner did not cancel/join/replace")
	}
}

func TestHTTPExecOwnerShutdownDoesNotReplace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	owner := &processExecOwner{ctx: ctx, grace: time.Millisecond, cancels: make(map[uint64]context.CancelFunc), exec: func(string, []string, []string) error { t.Error("replaced after shutdown"); return nil }}
	owner.request()
	deadline := time.Now().Add(time.Second)
	for owner.requested.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if owner.requested.Load() {
		t.Fatal("restart owner did not stop")
	}
}

func TestHTTPExecOwnerJoinsDetachedToolAfterResponseReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := &processExecOwner{ctx: ctx, grace: 40 * time.Millisecond, cancels: make(map[uint64]context.CancelFunc)}
	executed := make(chan struct{}, 1)
	owner.exec = func(string, []string, []string) error { executed <- struct{}{}; return errors.New("fixture") }
	var toolCtx context.Context
	var toolDone func()
	handler := owner.handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var err error
		toolCtx, toolDone, err = owner.child(context.WithoutCancel(r.Context()))
		if err != nil {
			t.Fatal(err)
		}
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
	owner.request()
	select {
	case <-toolCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("detached tool not cancelled")
	}
	select {
	case <-executed:
		t.Fatal("exec before actual tool return")
	default:
	}
	// A descendant started during cancellation must inherit cancellation too.
	lateCtx, lateDone, err := owner.child(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lateCtx.Err() == nil {
		t.Fatal("late descendant escaped cancellation")
	}
	toolDone()
	select {
	case <-executed:
		t.Fatal("exec before descendant return")
	default:
	}
	lateDone()
	select {
	case <-executed:
	case <-time.After(time.Second):
		t.Fatal("exec did not follow settlement")
	}
}

func TestHTTPExecOwnerUncooperativeToolPreventsReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := &processExecOwner{ctx: ctx, grace: 10 * time.Millisecond, cancels: make(map[uint64]context.CancelFunc), exec: func(string, []string, []string) error { t.Error("unsettled work was replaced"); return nil }}
	work, done, err := owner.enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	owner.request()
	deadline := time.Now().Add(time.Second)
	for owner.requested.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if owner.requested.Load() {
		t.Fatal("request never settled")
	}
	if work.Err() == nil {
		t.Fatal("work was not cancelled")
	}
	if owner.gate.Drained() {
		t.Fatal("cancellation mistaken for actual return")
	}
	// A failed attempt remains usable; the original operation is still accounted.
	_, release, err := owner.enter(ctx)
	if err != nil {
		t.Fatal("failed exec did not reopen admission", err)
	}
	release()
}
