package cmd

import (
	"net/http"
	"testing"
	"time"
)

type deadlineRecorder struct {
	http.ResponseWriter
	deadlines []time.Time
}

func (r *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	r.deadlines = append(r.deadlines, t)
	return nil
}

func (r *deadlineRecorder) Flush() {}

func TestStreamingResponseWriterSetsPerWriteDeadline(t *testing.T) {
	rec := &deadlineRecorder{ResponseWriter: noopResponseWriter{header: http.Header{}}}
	w := newStreamingResponseWriter(rec, time.Second)
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("streaming writer does not implement http.Flusher")
	}
	flusher.Flush()
	if len(rec.deadlines) != 4 {
		t.Fatalf("deadlines set = %d, want 4 (set+clear for Write and Flush)", len(rec.deadlines))
	}
	if rec.deadlines[0].IsZero() {
		t.Fatal("write deadline was not set")
	}
	if !rec.deadlines[1].IsZero() {
		t.Fatal("write deadline was not cleared after Write")
	}
	if rec.deadlines[2].IsZero() {
		t.Fatal("flush deadline was not set")
	}
	if !rec.deadlines[3].IsZero() {
		t.Fatal("write deadline was not cleared after Flush")
	}
}

type noopResponseWriter struct {
	header http.Header
}

func (w noopResponseWriter) Header() http.Header {
	return w.header
}

func (w noopResponseWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func (w noopResponseWriter) WriteHeader(statusCode int) {}
