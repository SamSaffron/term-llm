package wire

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestJSONWriteBudgetUsesEncodedSizeAndIsBounded(t *testing.T) {
	for _, tc := range []struct {
		size int
		want time.Duration
	}{
		{0, 10 * time.Second},
		{256 << 10, 11 * time.Second},
		// A20MiB original expands to~26.7MiB before JSON framing.
		{(20 << 20) * 4 / 3, 10*time.Second + time.Duration((20<<20)*4/3)*time.Second/(256<<10)},
		{100 << 20, 3 * time.Minute},
	} {
		if got := jsonWriteBudget(tc.size); got != tc.want {
			t.Fatalf("size=%d budget=%v want=%v", tc.size, got, tc.want)
		}
	}
}

type measuredJSONWriter struct {
	bytes.Buffer
	maxWrite int
	writes   int
}

func (w *measuredJSONWriter) Write(p []byte) (int, error) {
	w.maxWrite = max(w.maxWrite, len(p))
	w.writes++
	return w.Buffer.Write(p)
}

func TestBoundedJSONWriterPreservesBytesAndFragments(t *testing.T) {
	payload := bytes.Repeat([]byte("data"), 1<<20)
	var output measuredJSONWriter
	n, err := (boundedJSONWriter{Writer: &output}).Write(payload)
	if err != nil || n != len(payload) || !bytes.Equal(payload, output.Bytes()) || output.maxWrite > 32<<10 || output.writes < 2 {
		t.Fatalf("n=%d maxWrite=%d writes=%d error=%v", n, output.maxWrite, output.writes, err)
	}
}

type shortJSONWriter struct{}

func (shortJSONWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestBoundedJSONWriterRejectsShortWrites(t *testing.T) {
	if _, err := (boundedJSONWriter{Writer: shortJSONWriter{}}).Write([]byte("abc")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}

func TestFragmentedJSONCanceledBeforeFirstWireWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Nil connection deliberately panics if cancellation reaches any socket API.
	started, err := WriteFragmentedJSON(ctx, nil, map[string]string{"type": "request"})
	if started || !errors.Is(err, context.Canceled) {
		t.Fatalf("started=%v error=%v", started, err)
	}
}

func TestJSONChunkDeadlineCannotExceedControlWaitOrAbsoluteBudget(t *testing.T) {
	now := time.Now()
	if JSONChunkWriteWait > 10*time.Second {
		t.Fatalf("chunk deadline exceeds control-frame mutex wait: %v", JSONChunkWriteWait)
	}
	total := now.Add(3 * time.Minute)
	if got := jsonChunkDeadline(total, now); !got.Equal(now.Add(JSONChunkWriteWait)) {
		t.Fatalf("initial chunk deadline=%v", got)
	}
	// Progress refreshes the per-chunk timeout but never moves the absolute cap.
	nearEnd := total.Add(-time.Second)
	if got := jsonChunkDeadline(total, nearEnd); !got.Equal(total) {
		t.Fatalf("extended absolute message budget: %v", got)
	}
	if got := jsonChunkDeadline(total, total.Add(time.Second)); !got.Equal(total) {
		t.Fatalf("expired budget was revived: %v", got)
	}
	var output measuredJSONWriter
	refreshed := 0
	_, err := (boundedJSONWriter{Writer: &output, beforeWrite: func() { refreshed++ }}).Write(make([]byte, 3*jsonWriteChunkBytes+1))
	if err != nil || refreshed != 4 || output.writes != refreshed {
		t.Fatalf("per-fragment deadline refresh=%d writes=%d error=%v", refreshed, output.writes, err)
	}
}
