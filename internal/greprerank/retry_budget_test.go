package greprerank

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/classify/typesafe"
)

// The transport retains its existing two retries. All three requests inherit
// the operation cancellation, so neither retry waits nor in-flight HTTP can
// turn the remaining feature budget into a fresh per-request timeout.
func TestTransportTwoRetriesShareOperationBudget(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	backend, err := typesafe.NewClient(typesafe.Options{APIKey: "fake", BaseURL: server.URL, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	reranker := New(func() (classify.Backend, error) { return backend, nil }, "model", Options{Budget: 50 * time.Millisecond})
	started := time.Now()
	_, err = reranker.Rank(context.Background(), "intent", "pattern", mkItems(2))
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond+250*time.Millisecond {
		t.Fatalf("retried call exceeded budget: %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 3 {
		t.Fatalf("err=%v HTTP requests=%d, want timeout after 3 requests", err, calls.Load())
	}
	waitSemEmpty(t)
}
