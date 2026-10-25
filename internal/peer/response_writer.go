package peer

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

// responseWriter streams a handler's response as response_start,
// response_body and response_end frames. It implements http.Flusher so
// SSE handlers can push events immediately; writes block under DataChannel
// backpressure and fail once the request is cancelled or the channel closes.
type responseWriter struct {
	ctx  context.Context
	conn *conn
	id   string
	head bool

	mu      sync.Mutex
	header  http.Header
	status  int
	started bool
	ended   bool
	buf     []byte
	err     error
}

var _ http.Flusher = (*responseWriter)(nil)

func newResponseWriter(ctx context.Context, c *conn, id string, head bool) *responseWriter {
	return &responseWriter{ctx: ctx, conn: c, id: id, head: head, header: http.Header{}}
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.status != 0 || (code >= 100 && code < 200) {
		return
	}
	if code < 100 || code > 999 {
		code = http.StatusInternalServerError
	}
	w.status = code
}

func (w *responseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.startLocked(); err != nil {
		return 0, err
	}
	if w.head {
		return len(p), nil
	}
	chunk := w.conn.limits.BodyChunkBytes
	written := 0
	for len(p) > 0 {
		n := min(chunk-len(w.buf), len(p))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		if len(w.buf) == chunk {
			if err := w.flushLocked(); err != nil {
				return written, err
			}
		}
		written += n
	}
	return written, nil
}

// Flush sends the status line, headers and any buffered body now.
func (w *responseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.startLocked() == nil {
		_ = w.flushLocked()
	}
}

func (w *responseWriter) send(f *Frame) error {
	if w.err != nil {
		return w.err
	}
	if err := w.ctx.Err(); err != nil {
		w.err = err
		return err
	}
	if err := w.conn.send(w.ctx, f); err != nil {
		w.err = err
		return err
	}
	return nil
}

func (w *responseWriter) startLocked() error {
	if w.started {
		return w.err
	}
	w.started = true
	if w.status == 0 {
		w.status = http.StatusOK
	}
	err := w.send(&Frame{Type: TypeResponseStart, ID: w.id, Status: w.status, Header: sanitizeResponseHeader(w.header)})
	if errors.Is(err, errFrameTooLarge) {
		// Oversized headers: report a failure the requester can see.
		w.err = nil
		w.status = http.StatusInternalServerError
		if w.send(&Frame{Type: TypeResponseStart, ID: w.id, Status: w.status}) == nil {
			w.endLocked("response headers exceed frame limit")
		}
		w.err = err // later writes fail
	}
	return w.err
}

func (w *responseWriter) flushLocked() error {
	if len(w.buf) == 0 {
		return w.err
	}
	err := w.send(&Frame{Type: TypeResponseBody, ID: w.id, Body: w.buf})
	w.buf = w.buf[:0]
	return err
}

func (w *responseWriter) endLocked(errMsg string) {
	if w.ended {
		return
	}
	w.ended = true
	if w.ctx.Err() != nil {
		// Cancelled by the requester or the channel: nothing more is sent.
		return
	}
	_ = w.conn.send(w.ctx, &Frame{Type: TypeResponseEnd, ID: w.id, Error: errMsg})
}

// finish completes a response after the handler returns.
func (w *responseWriter) finish(errMsg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.startLocked() == nil {
		_ = w.flushLocked()
	}
	if w.err != nil && errMsg == "" && !errors.Is(w.err, context.Canceled) {
		errMsg = w.err.Error()
	}
	w.endLocked(errMsg)
}

// abort ends a response after a handler panic: a 500 if nothing was sent
// yet, otherwise a failed response_end so the truncation is visible.
func (w *responseWriter) abort(errMsg string) {
	w.mu.Lock()
	if !w.started {
		w.status = http.StatusInternalServerError
		w.header = http.Header{}
		w.buf = nil
	}
	w.mu.Unlock()
	w.finish(errMsg)
}
