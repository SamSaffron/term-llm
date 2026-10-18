package guardian

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

type blockingReviewerProvider struct {
	delegate *mockprovider.MockProvider
	started  chan<- struct{}
	release  <-chan struct{}
	cleaned  *atomic.Int32
}

func (p *blockingReviewerProvider) Name() string                    { return "blocking-reviewer" }
func (p *blockingReviewerProvider) Credential() string              { return "test" }
func (p *blockingReviewerProvider) Capabilities() core.Capabilities { return p.delegate.Capabilities() }
func (p *blockingReviewerProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	select {
	case p.started <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return p.delegate.Stream(ctx, req)
}
func (p *blockingReviewerProvider) CleanupMCP() {
	if p.cleaned != nil {
		p.cleaned.Add(1)
	}
}

func TestReviewerPoolBoundsConcurrencyAndLazilyExpands(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var factoryCalls atomic.Int32
	pool, err := NewReviewerPool(3, func() (*Reviewer, error) {
		factoryCalls.Add(1)
		provider := &blockingReviewerProvider{
			delegate: mockprovider.NewMockProvider("guardian").
				AddTextResponse(`{"outcome":"allow"}`).
				AddTextResponse(`{"outcome":"allow"}`),
			started: started,
			release: release,
		}
		return &Reviewer{Provider: provider, Model: "mock"}, nil
	})
	if err != nil {
		t.Fatalf("NewReviewerPool: %v", err)
	}
	defer pool.Close()
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("initial factory calls = %d, want one eager primary", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_, err := pool.Review(ctx, Request{Command: "echo ok"})
			errs <- err
		}()
	}
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatalf("only %d reviews entered providers: %v", i, ctx.Err())
		}
	}
	if got := factoryCalls.Load(); got != 3 {
		t.Fatalf("factory calls under contention = %d, want max 3", got)
	}
	select {
	case <-started:
		t.Fatal("fourth review exceeded pool concurrency")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatalf("fourth review never acquired released reviewer: %v", ctx.Err())
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("Review: %v", err)
		}
	}
}

func TestReviewerPoolWaiterCancellationAndCloseCleanup(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var cleaned atomic.Int32
	pool, err := NewReviewerPool(1, func() (*Reviewer, error) {
		return &Reviewer{Provider: &blockingReviewerProvider{
			delegate: mockprovider.NewMockProvider("guardian").AddTextResponse(`{"outcome":"allow"}`),
			started:  started,
			release:  release,
			cleaned:  &cleaned,
		}}, nil
	})
	if err != nil {
		t.Fatalf("NewReviewerPool: %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := pool.Review(context.Background(), Request{Command: "echo first"})
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first review did not start")
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pool.Review(waitCtx, Request{Command: "echo second"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiting review error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Review: %v", err)
	}
	pool.Close()
	pool.Close()
	if got := cleaned.Load(); got != 1 {
		t.Fatalf("provider cleanup calls = %d, want 1", got)
	}
	if _, err := pool.Review(context.Background(), Request{Command: "after close"}); err == nil {
		t.Fatal("Review succeeded after pool close")
	}
}
