package restart

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGeneralTransportFlushAndPassiveObserver(t *testing.T) {
	for _, passive := range []bool{false, true} {
		t.Run(map[bool]string{false: "response flush", true: "passive observer"}[passive], func(t *testing.T) {
			c := &Coordinator{Timeout: time.Second}
			transport := &HTTPTransport{Coordinator: c}
			conn, peer := net.Pipe()
			defer conn.Close()
			defer peer.Close()
			ctx := transport.ConnContext(context.Background(), conn)
			transport.ConnState(conn, http.StateActive)
			transport.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if passive {
					Passive(r.Context())
				}
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil).WithContext(ctx))
			exec := make(chan struct{}, 1)
			stop, err := c.Bind(context.Background(), func(context.Context) error { exec <- struct{}{}; return errors.New("fixture exec") })
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			c.Request()
			if !passive {
				select {
				case <-exec:
					t.Fatal("exec before response flushed")
				default:
				}
				// Controls remain available, but new root operations are rejected.
				transport.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if _, _, err := c.Root(r.Context()); !errors.Is(err, ErrDraining) {
						t.Errorf("root admitted: %v", err)
					}
				})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/stop", nil))
				transport.ConnState(conn, http.StateIdle)
			}
			select {
			case <-exec:
			case <-time.After(time.Second):
				t.Fatal("transport prevented reload")
			}
			waitAttempt(t, c)
			transport.ConnState(conn, http.StateClosed)
			if _, release, err := c.Root(context.Background()); err != nil {
				t.Fatal(err)
			} else {
				release()
			}
		})
	}
}

func TestTransportHijackedConnectionReleasesOwnership(t *testing.T) {
	c := &Coordinator{}
	transport := &HTTPTransport{Coordinator: c}
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	ctx := transport.ConnContext(context.Background(), conn)
	transport.ConnState(conn, http.StateActive)
	exec := make(chan struct{}, 1)
	stop, err := c.Bind(context.Background(), func(context.Context) error {
		exec <- struct{}{}
		return errors.New("fixture exec")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	transport.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		transport.ConnState(conn, http.StateHijacked)
		// Only the connection ticket is released; the handler still owns work.
		entry := ctx.Value(connectionKey{}).(*httpConnection)
		entry.mu.Lock()
		leaked := entry.release != nil
		entry.mu.Unlock()
		if leaked {
			t.Error("hijacked connection retained its activity ticket")
		}
		c.mu.Lock()
		active := c.active
		c.mu.Unlock()
		if active == 0 {
			t.Error("hijack released unfinished handler ownership")
		}
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil).WithContext(ctx))
	// net/http never reports StateClosed after StateHijacked.
	c.Request()
	select {
	case <-exec:
	case <-time.After(time.Second):
		t.Fatal("hijacked connection prevented reload after handler returned")
	}
	waitAttempt(t, c)
}

func TestHTTPTransportNewRequestsDuringDrain(t *testing.T) {
	for _, phase := range []string{"draining", "cancelling"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(phase+"/"+method, func(t *testing.T) {
				c := &Coordinator{draining: true, httpDeadline: time.Now().Add(time.Hour), interrupting: phase == "cancelling", status: Status{Phase: phase}}
				transport := &HTTPTransport{Coordinator: c}
				caller, cancel := context.WithCancelCause(context.Background())
				defer cancel(context.Canceled)
				callerErr := errors.New("caller stopped request")
				called := false
				transport.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					if deadline, ok := r.Context().Deadline(); !ok || !deadline.Equal(c.httpDeadline) {
						t.Fatalf("control deadline=%v/%v, want fixed %v", deadline, ok, c.httpDeadline)
					}
					if err := r.Context().Err(); err != nil {
						t.Fatalf("new request cancelled during %s: %v", phase, context.Cause(r.Context()))
					}
					// A request admitted during grace must also survive its expiry.
					c.interruptTasks(nil)
					if err := r.Context().Err(); err != nil {
						t.Fatalf("new drain request interrupted: %v", context.Cause(r.Context()))
					}
					if _, release, err := c.Root(r.Context()); !errors.Is(err, ErrDraining) {
						if release != nil {
							release()
						}
						t.Errorf("root admission = %v, want ErrDraining", err)
					}
					cancel(callerErr)
					if !errors.Is(context.Cause(r.Context()), callerErr) {
						t.Errorf("caller cancellation lost: %v", context.Cause(r.Context()))
					}
				})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/", nil).WithContext(caller))
				if !called {
					t.Fatal("drain request was not dispatched")
				}
			})
		}
	}
}

func TestHTTPTransportInterruptsOngoingRequest(t *testing.T) {
	c := &Coordinator{}
	transport := &HTTPTransport{Coordinator: c}
	transport.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Advance the coordinator synchronously, without a grace-period timer.
		c.mu.Lock()
		c.draining = true
		c.mu.Unlock()
		c.interruptTasks(nil)
		if !errors.Is(context.Cause(r.Context()), ErrInterrupt) {
			t.Errorf("ongoing request cause = %v, want ErrInterrupt", context.Cause(r.Context()))
		}
		c.mu.Lock()
		active := c.active
		c.mu.Unlock()
		if active != 1 {
			t.Errorf("interrupted handler ownership = %d, want 1", active)
		}
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if c.active != 0 || len(c.tasks) != 0 {
		t.Fatalf("handler leaked ownership: active=%d tasks=%d", c.active, len(c.tasks))
	}
}

// A control admitted after interruption must have time to run, but neither a
// long handler nor a stream of overlapping requests can renew that allowance.
func TestHTTPTransportDrainWindowCannotBeRenewed(t *testing.T) {
	c := &Coordinator{InterruptAfter: time.Hour}
	transport := &HTTPTransport{Coordinator: c}
	_, releaseBlocker, err := c.Root(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBlocker()
	replaced := make(chan struct{}, 1)
	stop, err := c.Bind(t.Context(), func(context.Context) error { replaced <- struct{}{}; return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	c.mu.Lock()
	c.interrupting = true
	c.httpDeadline = time.Now().Add(100 * time.Millisecond)
	c.mu.Unlock()
	caller, cancel := context.WithCancel(t.Context())
	cancelled, settled, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		cancel()
		select {
		case <-settled:
		default:
			close(settled)
		}
		<-done
	}()
	handler := transport.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Err() != nil {
			t.Error("control born cancelled")
		}
		<-r.Context().Done()
		if !errors.Is(context.Cause(r.Context()), ErrInterrupt) {
			t.Errorf("cause = %v", context.Cause(r.Context()))
		}
		close(cancelled)
		<-settled
	}))
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/control", nil).WithContext(caller))
	}()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("long drain request never interrupted")
	}
	var observerCtx context.Context
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		transport.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			observerCtx = r.Context()
			if r.Context().Err() != nil {
				t.Error("late control cancelled")
			}
			if !c.DrainingRequest(r.Context()) {
				t.Error("late producer fence missing")
			}
			if _, release, err := Child(r.Context()); !errors.Is(err, ErrReleased) {
				if release != nil {
					release()
				}
				t.Errorf("late observer inherited ownership: %v", err)
			}
			if _, release, err := c.Root(r.Context()); !errors.Is(err, ErrDraining) {
				if release != nil {
					release()
				}
				t.Errorf("late producer admitted: %v", err)
			}
		})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/read", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("late control status=%d", rec.Code)
		}
	}
	// Fresh HTTP/1 connections remain usable for controls, but do not take a
	// response-flush ticket after the deadline either.
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	transport.ConnContext(t.Context(), conn)
	transport.ConnState(conn, http.StateActive)
	entry := transport.connections[conn]
	entry.mu.Lock()
	hasTicket := entry.release != nil
	entry.mu.Unlock()
	// An unowned connection uses a no-op release, not an admission ticket.
	if !hasTicket {
		t.Fatal("late connection was closed rather than admitted")
	}
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active != 2 {
		t.Fatalf("cancelled handler/blocker ownership = %d, want 2", active)
	}
	releaseBlocker()
	select {
	case <-replaced:
		t.Fatal("replacement before cancelled handler settled")
	default:
	}
	lateStarted, lateDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(lateDone)
		transport.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(lateStarted); <-caller.Done() })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/late-control", nil))
	}()
	<-lateStarted
	defer func() { cancel(); <-lateDone }()
	close(settled)
	<-done
	select {
	case <-replaced:
	case <-time.After(time.Second):
		t.Fatal("steady requests prevented replacement")
	}
	waitAttempt(t, c)
	// A held observer must not become an unowned producer just because
	// rollback reopened ordinary admission.
	if !c.DrainingRequest(observerCtx) || c.DrainingRequest(t.Context()) {
		t.Fatal("rollback lost observer fence or blocked fresh requests")
	}
}

func TestHTTPTransportAdmissionRegistersInterruptBeforeDrain(t *testing.T) {
	c := &Coordinator{InterruptAfter: time.Millisecond}
	ctx, release, err := (&HTTPTransport{Coordinator: c}).AdmitRequest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	replaced := make(chan struct{}, 1)
	stop, err := c.Bind(t.Context(), func(context.Context) error { replaced <- struct{}{}; return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	// Drain starts between transport admission and application dispatch. The
	// request is already registered, never reclassified by a later Draining read.
	c.Request()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("pre-drain admission escaped interruption")
	}
	if !errors.Is(context.Cause(ctx), ErrInterrupt) {
		t.Fatalf("cause = %v", context.Cause(ctx))
	}
	select {
	case <-replaced:
		t.Fatal("interruption released request ownership")
	default:
	}
	release()
	select {
	case <-replaced:
	case <-time.After(time.Second):
		t.Fatal("released request prevented replacement")
	}
	waitAttempt(t, c)
}
