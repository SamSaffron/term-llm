package wire

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// SSEKeepalive starts a background goroutine that writes an SSE comment ping
// to w every interval while streaming is active. This prevents intermediate
// proxies (e.g. nginx with a short send_timeout) from closing the connection
// during silent periods — e.g. when the LLM is in extended thinking mode or
// the API is slow to emit tokens.
//
// The returned mu must wrap all writes to w inside the RunWithEvents callback.
// Call stop() immediately after RunWithEvents returns; it blocks until the
// goroutine has exited so subsequent final writes to w are safe without a lock.
// The goroutine also exits when ctx is cancelled between writes.
func SSEKeepalive(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, interval time.Duration) (mu *sync.Mutex, stop func()) {
	mu = &sync.Mutex{}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				_, _ = io.WriteString(w, ": ping\n\n")
				flusher.Flush()
				mu.Unlock()
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()
	return mu, func() {
		close(done)
		wg.Wait()
	}
}
