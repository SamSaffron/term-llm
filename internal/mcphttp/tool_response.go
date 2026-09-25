package mcphttp

import (
	"context"
	"io"
	"net/http"
	"sync"
)

type toolResponseKey struct{}

// Commit the SSE response when an accepted tool starts. Remove this workaround
// once the SDK includes https://github.com/modelcontextprotocol/go-sdk/pull/1197.
// Flushing before validation would prevent the SDK from returning HTTP errors.
func toolResponseMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := &toolResponse{ResponseWriter: w}
		defer func() {
			response.mu.Lock()
			response.closed = true
			response.mu.Unlock()
		}()
		ctx := context.WithValue(r.Context(), toolResponseKey{}, response)
		next.ServeHTTP(response, r.WithContext(ctx))
	})
}

// The SDK may write concurrently (for example, replies to a legacy batch).
// Keep the initial comment atomic with respect to its writes and flushes.
type toolResponse struct {
	http.ResponseWriter
	mu      sync.Mutex
	written bool
	closed  bool
}

func (w *toolResponse) start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	// A cancelled request can finish before its tool handler starts.
	if w.closed || w.written {
		return
	}
	w.written = true
	_, _ = io.WriteString(w.ResponseWriter, ": tool execution started\n\n")
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *toolResponse) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written = true
	return w.ResponseWriter.Write(p)
}

func (w *toolResponse) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *toolResponse) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written = true
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *toolResponse) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
