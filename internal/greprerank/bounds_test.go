package greprerank

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
)

func TestRelativePathsAreNotTruncated(t *testing.T) {
	path := strings.Repeat("目录/", 180) + "target.go"
	fb := &fakeBackend{fn: scoreBy(func(int) float64 { return .9 })}
	r := newRR(fb, Options{Threshold: .5})
	if _, err := r.Rank(context.Background(), "intent", "pattern", []Item{{path, "> one"}, {"other.go", "> two"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fb.reqs[0].State), path) {
		t.Fatal("relative path changed in classifier state")
	}
}

func TestSemaphoreSharedAcrossRerankers(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, MaxConcurrent)
	blocked := &fakeBackend{fn: func(ctx context.Context, req classify.Request) (*classify.Response, error) {
		entered <- struct{}{}
		<-release // deliberately ignore cancellation
		return scoreBy(func(int) float64 { return .9 })(ctx, req)
	}}
	first := newRR(blocked, Options{Threshold: .5, MaxCandidates: 500, Budget: 2 * time.Second})
	done := make(chan error, 1)
	go func() { _, err := first.Rank(context.Background(), "intent", "pattern", mkItems(200)); done <- err }()
	defer func() { close(release); <-done; waitSemEmpty(t) }()
	for range MaxConcurrent {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	otherBackend := &fakeBackend{fn: scoreBy(func(int) float64 { return .9 })}
	second := newRR(otherBackend, Options{Threshold: .5, Budget: 30 * time.Millisecond})
	start := time.Now()
	_, err := second.Rank(context.Background(), "intent", "pattern", mkItems(2))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Rank error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > second.Budget()+500*time.Millisecond {
		t.Fatalf("admission took %s", elapsed)
	}
	if otherBackend.calls() != 0 {
		t.Fatal("a separate reranker bypassed process-wide admission")
	}
}

func TestParentDeadlineDoesNotTripBreaker(t *testing.T) {
	var succeed atomic.Bool
	fb := &fakeBackend{fn: func(ctx context.Context, req classify.Request) (*classify.Response, error) {
		if succeed.Load() {
			return scoreBy(func(int) float64 { return .9 })(ctx, req)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r := newRR(fb, Options{Threshold: .5, Budget: time.Second})
	for range 4 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		_, err := r.Rank(ctx, "intent", "pattern", mkItems(2))
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
	waitSemEmpty(t)
	succeed.Store(true)
	if _, err := r.Rank(context.Background(), "intent", "pattern", mkItems(2)); err != nil {
		t.Fatalf("parent deadline tripped breaker: %v", err)
	}
}
