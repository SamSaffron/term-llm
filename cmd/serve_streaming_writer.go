package cmd

import (
	"net/http"
	"time"
)

const (
	serveReadHeaderTimeout     = 10 * time.Second
	serveIdleTimeout           = 2 * time.Minute
	serveStreamWriteTimeout    = 30 * time.Second
	durableResponseLookupLimit = 5 * time.Second
)

type streamingResponseWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
}

func newStreamingResponseWriter(w http.ResponseWriter, timeout time.Duration) http.ResponseWriter {
	if timeout <= 0 {
		return w
	}
	return &streamingResponseWriter{
		ResponseWriter: w,
		controller:     http.NewResponseController(w),
		timeout:        timeout,
	}
}

func (w *streamingResponseWriter) Write(p []byte) (int, error) {
	w.setDeadline()
	defer w.clearDeadline()
	return w.ResponseWriter.Write(p)
}

func (w *streamingResponseWriter) Flush() {
	w.setDeadline()
	defer w.clearDeadline()
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *streamingResponseWriter) setDeadline() {
	if w == nil || w.controller == nil || w.timeout <= 0 {
		return
	}
	_ = w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
}

func (w *streamingResponseWriter) clearDeadline() {
	if w == nil || w.controller == nil || w.timeout <= 0 {
		return
	}
	_ = w.controller.SetWriteDeadline(time.Time{})
}
