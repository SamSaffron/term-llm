package testutil

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

type noopResponseWriter struct{ header http.Header }

func (w noopResponseWriter) Header() http.Header         { return w.header }
func (w noopResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w noopResponseWriter) WriteHeader(int)             {}

// AssertStreamingWriterDeadlines verifies the shared write/flush deadline contract.
func AssertStreamingWriterDeadlines(t *testing.T, wrap func(http.ResponseWriter, time.Duration) http.ResponseWriter) {
	t.Helper()
	recorder := &deadlineRecorder{ResponseWriter: noopResponseWriter{header: http.Header{}}}
	writer := wrap(recorder, time.Second)
	if _, err := writer.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		t.Fatal("streaming writer does not implement http.Flusher")
	}
	flusher.Flush()
	if len(recorder.deadlines) != 4 {
		t.Fatalf("deadlines set = %d, want 4 (set+clear for Write and Flush)", len(recorder.deadlines))
	}
	for i, wantZero := range []bool{false, true, false, true} {
		if recorder.deadlines[i].IsZero() != wantZero {
			t.Fatalf("deadline %d zero = %v, want %v", i, recorder.deadlines[i].IsZero(), wantZero)
		}
	}
}
