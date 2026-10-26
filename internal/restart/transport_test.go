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
