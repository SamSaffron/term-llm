package serve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

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
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// A real socket hook holds the writer immediately after all bytes of this
			// small message reached the peer, before writeFrame returns to do(). This
			// replaces the removed caller-AfterFunc callback with an IO ordering barrier.
			wrote, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			hook := func() { close(wrote); <-release }
			c.conn.UnderlyingConn().(*containmentConn).afterWrite.Store(&hook)
			// Keep body-read cleanup independent of the frame-write boundary.
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
			case <-wrote:
			case <-time.After(2 * time.Second):
				t.Fatal("socket completion hook was not reached")
			}
			if responseReady {
				if err := peer.WriteJSON(hubReverseResponse{Type: hubReverseFrameResponseStart, ID: sent.ID, Status: 200}); err != nil {
					t.Fatal(err)
				}
				// Hold writeFrame at the socket completion boundary until the response lane
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
			cancel()
			close(release)
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
