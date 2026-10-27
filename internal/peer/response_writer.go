package peer

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
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
	bytes   uint64
	trace   *requestDiagnostics

	diagnosticQueue    [2]DiagnosticEvent
	diagnosticHead     int
	diagnosticCount    int
	diagnosticDraining bool
}

var _ http.Flusher = (*responseWriter)(nil)

func newResponseWriter(ctx context.Context, c *conn, id string, head bool, trace *requestDiagnostics) *responseWriter {
	return &responseWriter{ctx: ctx, conn: c, id: id, head: head, header: http.Header{}, trace: trace}
}

type requestDiagnostics struct {
	observer     DiagnosticObserver
	now          func() time.Time
	startedAt    time.Time
	method       DiagnosticMethod
	route        DiagnosticRoute
	request      uint64
	started      atomic.Bool
	peerCanceled atomic.Bool
}

func (s *Server) newRequestDiagnostics(frame *Frame) *requestDiagnostics {
	if s.cfg.Observer == nil {
		return nil
	}
	return &requestDiagnostics{
		observer: s.cfg.Observer, now: s.cfg.Now, method: diagnosticMethod(frame.Method),
		route: diagnosticRoute(frame.Path), request: s.nextRequest.Add(1),
	}
}

func (d *requestDiagnostics) start() {
	d.startedAt = d.now()
	d.started.Store(true)
	d.observer(DiagnosticEvent{
		Stage: DiagnosticRequestStart, Outcome: DiagnosticOutcomeStarted,
		Method: d.method, Route: d.route, Request: d.request,
	})
}

func (d *requestDiagnostics) headers(status int) (DiagnosticEvent, bool) {
	if d == nil || !d.started.Load() {
		return DiagnosticEvent{}, false
	}
	return DiagnosticEvent{
		Stage: DiagnosticResponseHeaders, Outcome: DiagnosticOutcomeStarted,
		Method: d.method, Route: d.route, Request: d.request, Status: status,
	}, true
}

func (d *requestDiagnostics) terminal(status int, bytes uint64, sendErr error, handlerFailed bool, ctxErr error) (DiagnosticEvent, bool) {
	if d == nil || !d.started.Load() {
		return DiagnosticEvent{}, false
	}
	stage, outcome, reason := DiagnosticRequestEnd, DiagnosticOutcomeFinished, DiagnosticReasonNone
	switch {
	case d.peerCanceled.Load():
		stage, outcome, reason = DiagnosticRequestCancel, DiagnosticOutcomeCanceled, DiagnosticReasonPeerCanceled
	case ctxErr != nil && errors.Is(sendErr, ErrClosed):
		stage, outcome, reason = DiagnosticRequestFailure, DiagnosticOutcomeTruncated, DiagnosticReasonChannelClosed
	case ctxErr != nil:
		stage, outcome, reason = DiagnosticRequestCancel, DiagnosticOutcomeCanceled, DiagnosticReasonContextCanceled
	case sendErr != nil:
		stage, outcome, reason = DiagnosticRequestFailure, DiagnosticOutcomeTruncated, DiagnosticReasonSendFailed
	case handlerFailed:
		stage, outcome, reason = DiagnosticRequestFailure, DiagnosticOutcomeFailed, DiagnosticReasonHandlerFailed
	}
	duration := d.now().Sub(d.startedAt)
	if duration < 0 {
		duration = 0
	}
	return DiagnosticEvent{
		Stage: stage, Outcome: outcome, Reason: reason, Method: d.method, Route: d.route,
		Request: d.request, Status: status, Bytes: bytes, Duration: duration,
	}, true
}

func (w *responseWriter) queueDiagnosticLocked(event DiagnosticEvent, ok bool) {
	if !ok {
		return
	}
	if w.diagnosticCount == len(w.diagnosticQueue) {
		panic("peer: diagnostic event queue overflow")
	}
	index := (w.diagnosticHead + w.diagnosticCount) % len(w.diagnosticQueue)
	w.diagnosticQueue[index] = event
	w.diagnosticCount++
}

func (w *responseWriter) drainDiagnostics() {
	if w.trace == nil {
		return
	}
	w.mu.Lock()
	if w.diagnosticDraining {
		w.mu.Unlock()
		return
	}
	w.diagnosticDraining = true
	w.mu.Unlock()

	for {
		w.mu.Lock()
		if w.diagnosticCount == 0 {
			w.diagnosticDraining = false
			w.mu.Unlock()
			return
		}
		event := w.diagnosticQueue[w.diagnosticHead]
		w.diagnosticQueue[w.diagnosticHead] = DiagnosticEvent{}
		w.diagnosticHead = (w.diagnosticHead + 1) % len(w.diagnosticQueue)
		w.diagnosticCount--
		w.mu.Unlock()
		w.trace.observer(event)
	}
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
	if err := w.startLocked(); err != nil {
		w.mu.Unlock()
		w.drainDiagnostics()
		return 0, err
	}
	if w.head {
		w.mu.Unlock()
		w.drainDiagnostics()
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
				w.mu.Unlock()
				w.drainDiagnostics()
				return written, err
			}
		}
		written += n
	}
	w.mu.Unlock()
	w.drainDiagnostics()
	return written, nil
}

// Flush sends the status line, headers and any buffered body now.
func (w *responseWriter) Flush() {
	w.mu.Lock()
	if w.startLocked() == nil {
		_ = w.flushLocked()
	}
	w.mu.Unlock()
	w.drainDiagnostics()
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
	if err == nil {
		w.queueDiagnosticLocked(w.trace.headers(w.status))
	}
	if errors.Is(err, errFrameTooLarge) {
		// Oversized headers: report a failure the requester can see.
		w.err = nil
		w.status = http.StatusInternalServerError
		if w.send(&Frame{Type: TypeResponseStart, ID: w.id, Status: w.status}) == nil {
			w.queueDiagnosticLocked(w.trace.headers(w.status))
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
	if err == nil {
		w.bytes = min(w.bytes+uint64(len(w.buf)), uint64(1<<40))
	}
	w.buf = w.buf[:0]
	return err
}

func (w *responseWriter) endLocked(errMsg string) {
	if w.ended {
		return
	}
	w.ended = true
	if ctxErr := w.ctx.Err(); ctxErr != nil {
		sendErr := w.err
		select {
		case <-w.conn.closed:
			sendErr = ErrClosed
		default:
		}
		w.queueDiagnosticLocked(w.trace.terminal(w.status, w.bytes, sendErr, errMsg != "", ctxErr))
		// Cancelled by the requester or the channel: nothing more is sent.
		return
	}
	if err := w.conn.send(w.ctx, &Frame{Type: TypeResponseEnd, ID: w.id, Error: errMsg}); err != nil && w.err == nil {
		w.err = err
	}
	w.queueDiagnosticLocked(w.trace.terminal(w.status, w.bytes, w.err, errMsg != "", nil))
}

// finish completes a response after the handler returns.
func (w *responseWriter) finish(errMsg string) {
	w.mu.Lock()
	if w.startLocked() == nil {
		_ = w.flushLocked()
	}
	if w.err != nil && errMsg == "" && !errors.Is(w.err, context.Canceled) {
		errMsg = w.err.Error()
	}
	w.endLocked(errMsg)
	w.mu.Unlock()
	w.drainDiagnostics()
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
