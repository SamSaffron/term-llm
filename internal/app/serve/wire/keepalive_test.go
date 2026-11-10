package wire

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestSSEKeepalive(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		name := "stop"
		if cancelContext {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				recorder := httptest.NewRecorder()
				_, stop := SSEKeepalive(ctx, recorder, recorder, 10*time.Second)
				stop = sync.OnceFunc(stop)
				defer stop()
				synctest.Wait()

				time.Sleep(10 * time.Second) // Virtual time inside the synctest bubble.
				synctest.Wait()
				if got := recorder.Body.String(); got != ": ping\n\n" || !recorder.Flushed {
					t.Fatalf("body = %q, flushed = %v", got, recorder.Flushed)
				}

				if cancelContext {
					cancel()
				} else {
					stop()
				}
				synctest.Wait()
				time.Sleep(30 * time.Second)
				synctest.Wait()
				if got := recorder.Body.String(); got != ": ping\n\n" {
					t.Fatalf("keepalive continued after %s: %q", name, got)
				}
			})
		})
	}
}
