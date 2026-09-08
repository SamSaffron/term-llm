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
