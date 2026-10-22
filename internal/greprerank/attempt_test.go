package greprerank

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/classify/typesafe"
)

func okBackend() *fakeBackend {
	return &fakeBackend{fn: scoreBy(func(int) float64 { return 1 })}
}

func TestAttemptBudgetTimeoutBeforeRankTripsBreaker(t *testing.T) {
	r := newRR(okBackend(), Options{Budget: 20 * time.Millisecond})
	for i := 0; i < breakerFailures; i++ {
		a, err := r.Begin(context.Background())
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		a.Start()
		<-a.Context().Done() // expansion consumed the whole budget
		if err := a.Finish(a.Context().Err()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := r.Begin(context.Background()); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("breaker not open: %v", err)
	}
}

func TestAttemptBeginStartsNoClock(t *testing.T) {
	r := newRR(okBackend(), Options{Budget: 10 * time.Millisecond})
	a, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := a.Context().Err(); err != nil {
		t.Fatalf("context expired without Start: %v", err)
	}
	if err := a.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if a.Context().Err() == nil {
		t.Fatal("Finish must cancel the worker context")
	}
}

func TestAttemptStartOnceKeepsRemainingBudget(t *testing.T) {
	r := newRR(okBackend(), Options{Budget: 300 * time.Millisecond})
	a, _ := r.Begin(context.Background())
	t0 := time.Now()
	a.Start()
	time.Sleep(150 * time.Millisecond)
	a.Start() // must not reset the timer
	<-a.Context().Done()
	if el := time.Since(t0); el > 400*time.Millisecond {
		t.Fatalf("budget was reset: %v", el)
	}
	if err := a.Finish(nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired attempt must not succeed: %v", err)
	}
}

func TestAttemptRankDoesNotResetBudget(t *testing.T) {
	fb := &fakeBackend{fn: func(ctx context.Context, _ classify.Request) (*classify.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r := newRR(fb, Options{Budget: 200 * time.Millisecond})
	a, _ := r.Begin(context.Background())
	a.Start()
	time.Sleep(120 * time.Millisecond)
	t0 := time.Now()
	_, err := a.Rank("i", "p", mkItems(3))
	if el := time.Since(t0); el > 160*time.Millisecond {
		t.Fatalf("Rank got a fresh budget: %v", el)
	}
	if err = a.Finish(err); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestAttemptParentCancelNeutral(t *testing.T) {
	r := newRR(okBackend(), Options{Budget: time.Second})
	for i := 0; i < 2*breakerFailures; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		a, err := r.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		a.Start()
		cancel()
		if err := a.Finish(a.Context().Err()); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	// Parent deadline is also neutral.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	a, _ := r.Begin(ctx)
	a.Start()
	<-a.Context().Done()
	if err := a.Finish(a.Context().Err()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	a, err := r.Begin(context.Background())
	if err != nil {
		t.Fatalf("breaker tripped by neutral outcomes: %v", err)
	}
	a.Finish(ErrInput)
}

func TestAttemptFinishRecordsOnce(t *testing.T) {
	r := newRR(okBackend(), Options{Budget: time.Second})
	for i := 0; i < 5; i++ {
		a, err := r.Begin(context.Background())
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		a.Start()
		e1 := a.Finish(errors.New("secret backend text"))
		e2 := a.Finish(nil)
		if !errors.Is(e1, ErrClassifier) || e1 != e2 {
			t.Fatalf("e1=%v e2=%v", e1, e2)
		}
		if i < breakerFailures-1 {
			continue
		}
		break
	}
	// Five double-Finish calls recorded at most one failure each; the breaker
	// must open after exactly breakerFailures attempts.
	r2 := newRR(okBackend(), Options{Budget: time.Second})
	for i := 0; i < breakerFailures-1; i++ {
		a, _ := r2.Begin(context.Background())
		a.Finish(errors.New("x"))
		a.Finish(errors.New("x"))
	}
	if a, err := r2.Begin(context.Background()); err != nil {
		t.Fatalf("double Finish counted twice: %v", err)
	} else {
		a.Finish(nil)
	}
}

func TestAttemptNeutralAndNormalised(t *testing.T) {
	r := newRR(okBackend(), Options{})
	for _, tc := range []struct{ in, want error }{
		{ErrInput, ErrInput},
		{ErrBreakerOpen, ErrBreakerOpen},
		{ErrUnavailable, ErrUnavailable},
		{errors.New("leaky http://x"), ErrClassifier},
	} {
		a, _ := r.Begin(context.Background())
		if got := a.Finish(tc.in); got != tc.want {
			t.Fatalf("%v -> %v", tc.in, got)
		}
	}
	// Only the last two count; skips must not have tripped anything.
	if _, err := r.Begin(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAttemptStartConcurrent(t *testing.T) {
	r := newRR(okBackend(), Options{Budget: 20 * time.Millisecond})
	a, _ := r.Begin(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Start()
			_ = a.Context().Err()
			a.Finish(nil)
		}()
	}
	wg.Wait()
}

func TestAttemptRankSuccessResetsBreaker(t *testing.T) {
	fb := &fakeBackend{fn: func(context.Context, classify.Request) (*classify.Response, error) {
		return nil, errors.New("boom")
	}}
	r := newRR(fb, Options{})
	for i := 0; i < breakerFailures-1; i++ {
		if _, err := r.Rank(context.Background(), "i", "p", mkItems(3)); !errors.Is(err, ErrClassifier) {
			t.Fatal(err)
		}
	}
	fb.fn = scoreBy(func(int) float64 { return 1 })
	a, _ := r.Begin(context.Background())
	a.Start()
	res, err := a.Rank("i", "p", mkItems(3))
	if err = a.Finish(err); err != nil || len(res.Kept) != 3 {
		t.Fatal(err, res)
	}
	fb.fn = func(context.Context, classify.Request) (*classify.Response, error) { return nil, errors.New("boom") }
	for i := 0; i < breakerFailures-1; i++ {
		r.Rank(context.Background(), "i", "p", mkItems(3))
	}
	if _, err := r.Begin(context.Background()); err != nil {
		t.Fatalf("success did not reset failures: %v", err)
	}
}

func TestBuildRequestsHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := buildRequests(ctx, "m", "i", "p", mkItems(120)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := buildRequests(context.Background(), "m", "i", "p", mkItems(120)); err != nil {
		t.Fatal(err)
	}
}

func TestAttemptRankExpiredContextNoBackendCall(t *testing.T) {
	fb := okBackend()
	r := newRR(fb, Options{Budget: time.Millisecond})
	a, _ := r.Begin(context.Background())
	a.Start()
	<-a.Context().Done()
	_, err := a.Rank("i", "p", mkItems(3))
	if err == nil || fb.calls() != 0 {
		t.Fatal(err, fb.calls())
	}
	if err = a.Finish(err); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

// A real typesafe backend whose server demands a long Retry-After must be
// abandoned promptly: the transport never sleeps past the operation deadline.
func TestTransportRetryAfterBeyondBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	client, err := typesafe.NewClient(typesafe.Options{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	r := New(func() (classify.Backend, error) { return client, nil }, "m", Options{Budget: 50 * time.Millisecond})
	start := time.Now()
	_, err = r.Rank(context.Background(), "i", "p", mkItems(3))
	if el := time.Since(start); el > 50*time.Millisecond+250*time.Millisecond {
		t.Fatalf("Rank outlived budget: %v", el)
	}
	// The transport gives up at once rather than sleeping past the deadline;
	// the 429 is a backend failure (not a timeout), so it is a classifier error.
	if !errors.Is(err, ErrClassifier) || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	waitSemEmpty(t)
}

func TestSkippedAndInputAreNeutralAndDistinct(t *testing.T) {
	r := newRR(okBackend(), Options{MaxCandidates: 5})
	for i := 0; i < 2*breakerFailures; i++ {
		a, err := r.Begin(context.Background())
		if err != nil {
			t.Fatalf("breaker tripped: %v", err)
		}
		a.Start()
		_, rerr := a.Rank("i", "p", mkItems(1))
		if got := a.Finish(rerr); got != ErrSkipped {
			t.Fatalf("skip: %v", got)
		}
		a, _ = r.Begin(context.Background())
		_, rerr = a.Rank("i", "p", mkItems(6))
		if got := a.Finish(rerr); got != ErrInput || errors.Is(got, ErrSkipped) {
			t.Fatalf("input: %v", got)
		}
	}
}

func TestAttemptParentCancelWithNilResultIsNeutral(t *testing.T) {
	fb := &fakeBackend{fn: func(context.Context, classify.Request) (*classify.Response, error) {
		return nil, errors.New("boom")
	}}
	r := newRR(fb, Options{Budget: time.Second})
	for i := 0; i < breakerFailures-1; i++ {
		r.Rank(context.Background(), "i", "p", mkItems(3))
	}
	// A raced success under a cancelled parent must neither reset nor count.
	ctx, cancel := context.WithCancel(context.Background())
	a, _ := r.Begin(ctx)
	a.Start()
	cancel()
	if err := a.Finish(nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	r.mu.Lock()
	fails := r.fails
	r.mu.Unlock()
	if fails != breakerFailures-1 {
		t.Fatalf("fails=%d, want %d", fails, breakerFailures-1)
	}
}

func TestAttemptCauseOrdering(t *testing.T) {
	// Parent deadline first: neutral even though Start was called.
	r := newRR(okBackend(), Options{Budget: time.Hour})
	for i := 0; i < 2*breakerFailures; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		a, err := r.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		a.Start()
		<-a.Context().Done()
		if err := a.Finish(a.Context().Err()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		cancel()
	}
	// Own budget first, parent cancelled afterwards: still counts.
	r2 := newRR(okBackend(), Options{Budget: 10 * time.Millisecond})
	for i := 0; i < breakerFailures; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		a, err := r2.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		a.Start()
		<-a.Context().Done()
		cancel()
		if err := a.Finish(nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
	if _, err := r2.Begin(context.Background()); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("budget failures not counted: %v", err)
	}
	if _, err := r.Begin(context.Background()); err != nil {
		t.Fatalf("parent deadlines counted: %v", err)
	}
}
