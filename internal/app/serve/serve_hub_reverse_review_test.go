package serve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Use Context's AfterFunc hook to cancel exactly after the frame-write callback
// is unregistered, not at an Err-call count that changes with implementation.
// The underlying context is genuinely canceled; the socket is a real peer.
type hubCancelAfterStopContext struct {
	context.Context
	cancel  context.CancelFunc
	stopped chan struct{}
	resume  chan struct{}
}

func (c *hubCancelAfterStopContext) Value(any) any { return nil }
func (c *hubCancelAfterStopContext) AfterFunc(f func()) func() bool {
	stop := context.AfterFunc(c.Context, f)
	return func() bool {
		ok := stop()
		if ok {
			close(c.stopped)
			<-c.resume
			c.cancel()
		}
		return ok
	}
}

func TestHubReverseCancelAfterSuccessfulWrite(t *testing.T) {
	for _, responseReady := range []bool{false, true} {
		name := "silent response"
		if responseReady {
			name = "response start also ready"
		}
		t.Run(name, func(t *testing.T) {
			s, _, node, peer, c := containmentHub(t)
			frames := make(chan hubReverseRequest, 8)
			go func() {
				for {
					var frame hubReverseRequest
					if peer.ReadJSON(&frame) != nil {
						return
					}
					frames <- frame
					if frame.Path == "/barrier" {
						_ = peer.WriteJSON(hubReverseResponse{ID: frame.ID, Status: 200, Body: []byte(`{}`)})
					}
				}
			}()
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &hubCancelAfterStopContext{Context: base, cancel: cancel, stopped: make(chan struct{}), resume: make(chan struct{})}
			var resumeOnce sync.Once
			resume := func() { resumeOnce.Do(func() { close(ctx.resume) }) }
			defer resume()
			// A nil body prevents unrelated body-read cleanup from invoking AfterFunc.
			req := httptest.NewRequest(http.MethodPost, "http://reverse.local/late", nil).WithContext(ctx)
			req.Body = nil
			done := make(chan error, 1)
			go func() {
				resp, err := s.reverse.do(ctx, node, req)
				if resp != nil {
					_ = resp.Body.Close()
				}
				done <- err
			}()
			var sent hubReverseRequest
			select {
			case sent = <-frames:
			case <-time.After(2 * time.Second):
				t.Fatal("request did not reach peer")
			}
			select {
			case <-ctx.stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("write callback was not stopped")
			}
			if responseReady {
				if err := peer.WriteJSON(hubReverseResponse{Type: hubReverseFrameResponseStart, ID: sent.ID, Status: 200}); err != nil {
					t.Fatal(err)
				}
				// Hold writeFrame at the stopped-callback boundary until the response lane
				// is ready too, then cancel. The response branch must not lose cancellation.
				watchdog := time.After(2 * time.Second)
				for {
					c.pendingMu.Lock()
					p := c.pending[sent.ID]
					ready := p != nil && len(p.ch) > 0
					c.pendingMu.Unlock()
					if ready {
						break
					}
					select {
					case <-watchdog:
						t.Fatal("response_start not queued")
					default:
						runtime.Gosched()
					}
				}
			}
			resume()
			if err := containmentAwait(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("request error=%v", err)
			}
			if err := containmentAwait(t, containmentDo(s, t.Context(), node, "/barrier", nil)); err != nil {
				t.Fatalf("healthy barrier: %v", err)
			}
			sawCancel := false
			watchdog := time.After(2 * time.Second)
			for !sawCancel {
				select {
				case frame := <-frames:
					sawCancel = frame.Type == hubReverseFrameCancel && frame.ID == sent.ID
				case <-c.closed:
					t.Fatal("completed-frame cancellation unnecessarily closed tunnel")
				case <-watchdog:
					t.Fatalf("delivered request %s was orphaned: no cancel after healthy barrier", sent.ID)
				}
			}
			c.pendingMu.Lock()
			count := len(c.pending)
			c.pendingMu.Unlock()
			if count != 0 || len(c.active) != 0 {
				t.Fatalf("local work retained: pending=%d active=%d", count, len(c.active))
			}
		})
	}
}
