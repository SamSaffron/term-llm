package greprerank

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/classify"
)

type fakeBackend struct {
	mu   sync.Mutex
	reqs []classify.Request
	fn   func(ctx context.Context, req classify.Request) (*classify.Response, error)
}

func (f *fakeBackend) Classify(ctx context.Context, req classify.Request) (*classify.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	return f.fn(ctx, req)
}
func (f *fakeBackend) ListModels(context.Context) (*classify.ModelsResponse, error) { return nil, nil }
func (f *fakeBackend) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func noul(v float64) classify.Answer { return classify.Answer{Type: classify.TypeNoul, Noul: &v} }

// scoreBy answers every question with score(globalNumber).
func scoreBy(score func(n int) float64) func(context.Context, classify.Request) (*classify.Response, error) {
	return func(_ context.Context, req classify.Request) (*classify.Response, error) {
		ans := map[string]classify.Answer{}
		for id := range req.Questions {
			n, _ := strconv.Atoi(strings.TrimPrefix(id, "r"))
			ans[id] = noul(score(n))
		}
		return &classify.Response{Answers: ans}, nil
	}
}

func newRR(b classify.Backend, opts Options) *Reranker {
	if opts.Budget == 0 {
		opts.Budget = 2 * time.Second
	}
	return New(func() (classify.Backend, error) { return b, nil }, "m", opts)
}

func mkItems(n int) []Item {
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{Path: fmt.Sprintf("f%d.go", i+1), Block: fmt.Sprintf("> line %d", i+1)}
	}
	return items
}

func TestRequestBuilding(t *testing.T) {
	for _, n := range []int{2, 49, 50, 51, 150} {
		fb := &fakeBackend{fn: scoreBy(func(int) float64 { return 1 })}
		r := newRR(fb, Options{Threshold: .5, MaxCandidates: 150})
		if _, err := r.Rank(context.Background(), "find <x> “é”", "pat\"", mkItems(n)); err != nil {
			t.Fatal(n, err)
		}
		if want := (n + 49) / 50; fb.calls() != want {
			t.Fatalf("n=%d calls=%d want %d", n, fb.calls(), want)
		}
		seen := map[string]bool{}
		for _, req := range fb.reqs {
			if len(req.Questions) > 50 || req.Model != "m" {
				t.Fatalf("bad request %d %q", len(req.Questions), req.Model)
			}
			var st map[string]string
			if err := json.Unmarshal(req.State, &st); err != nil {
				t.Fatal(err)
			}
			if st["search_intent"] != "find <x> “é”" || st["grep_pattern"] != "pat\"" || len(st) != 3 {
				t.Fatalf("state %v", st)
			}
			if !strings.Contains(string(req.State), "<x>") {
				t.Fatal("html escaped")
			}
			for id, q := range req.Questions {
				num, _ := strconv.Atoi(id[1:])
				var ins string
				_ = json.Unmarshal(q.Instructions, &ins)
				want := fmt.Sprintf("Is search result #%d relevant to the search intent, i.e. something the agent actually wants to see (not an incidental pattern match)?", num)
				if q.Type != classify.TypeNoul || ins != want {
					t.Fatalf("question %s: %+v %q", id, q, ins)
				}
				if !strings.Contains(st["results"], fmt.Sprintf("%d. f%d.go\n> line %d", num, num, num)) {
					t.Fatalf("missing %d in results", num)
				}
				seen[id] = true
			}
			if err := req.Validate(); err != nil {
				t.Fatal(err)
			}
		}
		if len(seen) != n {
			t.Fatalf("n=%d seen=%d", n, len(seen))
		}
	}
}

func TestResultsFormatMatchesEval(t *testing.T) {
	fb := &fakeBackend{fn: scoreBy(func(int) float64 { return 1 })}
	r := newRR(fb, Options{})
	items := []Item{{"a.go", "> one\n two"}, {"b.go", "> three"}}
	if _, err := r.Rank(context.Background(), "i", "p", items); err != nil {
		t.Fatal(err)
	}
	var st map[string]string
	_ = json.Unmarshal(fb.reqs[0].State, &st)
	if want := "1. a.go\n> one\n two\n\n2. b.go\n> three"; st["results"] != want {
		t.Fatalf("%q", st["results"])
	}
}

func TestBoundBlock(t *testing.T) {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf(" ctx %d", i))
	}
	lines[25] = "> the match"
	got := BoundBlock(strings.Join(lines, "\n"))
	if n := len(strings.Split(got, "\n")); n != 12 || !strings.Contains(got, "> the match") {
		t.Fatalf("lines=%d %q", n, got)
	}
	// Order preserved.
	if strings.Index(got, "ctx 0") > strings.Index(got, "> the match") {
		t.Fatal("order")
	}
	long := "> " + strings.Repeat("é世", 2000)
	got = BoundBlock(" before\n" + long + "\n after")
	if utf8.RuneCountInString(got) > 1200 || !utf8.ValidString(got) || !strings.Contains(got, "> é世") {
		t.Fatalf("runes=%d", utf8.RuneCountInString(got))
	}
	many := strings.Repeat("> m\n", 40)
	if n := len(strings.Split(BoundBlock(many), "\n")); n != 12 {
		t.Fatal(n)
	}
	plain := strings.Repeat("x", 5000)
	if utf8.RuneCountInString(BoundBlock(plain)) != 1200 {
		t.Fatal("plain")
	}
	if got := BoundBlock("a\xffb"); !utf8.ValidString(got) {
		t.Fatal("invalid utf8")
	}
	if got := BoundBlock("> x\n y"); got != "> x\n y" {
		t.Fatal(got)
	}
}

func TestInputRejected(t *testing.T) {
	fb := &fakeBackend{fn: scoreBy(func(int) float64 { return 1 })}
	r := newRR(fb, Options{MaxCandidates: 5})
	big := strings.Repeat("a", MaxPromptBytes+1)
	for name, tc := range map[string]struct {
		intent, pat string
		items       []Item
	}{
		"one item":     {"i", "p", mkItems(1)},
		"none":         {"i", "p", nil},
		"too many":     {"i", "p", mkItems(6)},
		"big intent":   {big, "p", mkItems(3)},
		"big pattern":  {"i", big, mkItems(3)},
		"empty intent": {"", "p", mkItems(3)},
	} {
		if _, err := r.Rank(context.Background(), tc.intent, tc.pat, tc.items); !errors.Is(err, ErrInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if fb.calls() != 0 {
		t.Fatal("backend called")
	}
}

func TestAccessors(t *testing.T) {
	r := New(nil, "m", Options{Threshold: .7, MaxCandidates: 9})
	if r.Threshold() != .7 || r.MaxCandidates() != 9 {
		t.Fatal()
	}
	if New(nil, "m", Options{}).MaxCandidates() != DefaultMaxCandidates {
		t.Fatal()
	}
}

func TestRankingOrderTiesThreshold(t *testing.T) {
	scores := map[int]float64{1: .5, 2: .9, 3: .5, 4: .2, 5: .9, 6: .49}
	fb := &fakeBackend{fn: scoreBy(func(n int) float64 { return scores[n] })}
	r := newRR(fb, Options{Threshold: .5})
	res, err := r.Rank(context.Background(), "i", "p", mkItems(6))
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 4, 0, 2}
	if fmt.Sprint(res.Kept) != fmt.Sprint(want) || res.Culled != 2 || res.BelowThresholdAll {
		t.Fatalf("%+v", res)
	}
	if fmt.Sprint(res.Scores) != "[0.9 0.9 0.5 0.5]" {
		t.Fatalf("%v", res.Scores)
	}
}

func TestAllBelowThresholdTop3(t *testing.T) {
	fb := &fakeBackend{fn: scoreBy(func(n int) float64 { return float64(n) / 100 })}
	r := newRR(fb, Options{Threshold: .5})
	res, err := r.Rank(context.Background(), "i", "p", mkItems(5))
	if err != nil {
		t.Fatal(err)
	}
	if !res.BelowThresholdAll || fmt.Sprint(res.Kept) != "[4 3 2]" || res.Culled != 2 {
		t.Fatalf("%+v", res)
	}
	r2 := newRR(&fakeBackend{fn: scoreBy(func(int) float64 { return 0 })}, Options{Threshold: .5})
	res, _ = r2.Rank(context.Background(), "i", "p", mkItems(2))
	if len(res.Kept) != 2 || fmt.Sprint(res.Kept) != "[0 1]" {
		t.Fatalf("%+v", res)
	}
}

func TestRankingAcrossChunks(t *testing.T) {
	fb := &fakeBackend{fn: scoreBy(func(n int) float64 {
		if n%2 == 0 {
			return 1
		}
		return 0
	})}
	r := newRR(fb, Options{Threshold: .5, MaxCandidates: 150})
	res, err := r.Rank(context.Background(), "i", "p", mkItems(120))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Kept) != 60 || res.Kept[0] != 1 || res.Kept[59] != 119 || res.Culled != 60 {
		t.Fatalf("%v", res.Kept)
	}
}

func TestValidationFailures(t *testing.T) {
	good := func(ids ...string) map[string]classify.Answer {
		m := map[string]classify.Answer{}
		for _, id := range ids {
			m[id] = noul(.9)
		}
		return m
	}
	nan, inf, neg, big := math.NaN(), math.Inf(1), -0.1, 1.1
	cases := map[string]*classify.Response{
		"nil":        nil,
		"missing":    {Answers: good("r1")},
		"extra":      {Answers: good("r1", "r2", "r3")},
		"unexpected": {Answers: good("r1", "r9")},
		"wrong type": {Answers: map[string]classify.Answer{"r1": noul(.9), "r2": {Type: classify.TypeScore, Noul: &nan}}},
		"nil noul":   {Answers: map[string]classify.Answer{"r1": noul(.9), "r2": {Type: classify.TypeNoul}}},
		"nan":        {Answers: map[string]classify.Answer{"r1": noul(.9), "r2": {Type: classify.TypeNoul, Noul: &nan}}},
		"inf":        {Answers: map[string]classify.Answer{"r1": noul(.9), "r2": {Type: classify.TypeNoul, Noul: &inf}}},
		"neg":        {Answers: map[string]classify.Answer{"r1": noul(.9), "r2": {Type: classify.TypeNoul, Noul: &neg}}},
		"big":        {Answers: map[string]classify.Answer{"r1": noul(.9), "r2": {Type: classify.TypeNoul, Noul: &big}}},
		"confidence": {Answers: map[string]classify.Answer{"r1": noul(.9), "r2": {Type: classify.TypeNoul, Confidence: &big}}},
	}
	for name, resp := range cases {
		fb := &fakeBackend{fn: func(context.Context, classify.Request) (*classify.Response, error) { return resp, nil }}
		r := newRR(fb, Options{})
		if _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); !errors.Is(err, ErrClassifier) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestBackendErrorHidesText(t *testing.T) {
	fb := &fakeBackend{fn: func(context.Context, classify.Request) (*classify.Response, error) {
		return nil, errors.New("secret-key-123")
	}}
	_, err := newRR(fb, Options{}).Rank(context.Background(), "i", "p", mkItems(3))
	if !errors.Is(err, ErrClassifier) || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}

func TestBackendPanicIsClassifierError(t *testing.T) {
	fb := &fakeBackend{fn: func(context.Context, classify.Request) (*classify.Response, error) { panic("boom") }}
	if _, err := newRR(fb, Options{}).Rank(context.Background(), "i", "p", mkItems(3)); !errors.Is(err, ErrClassifier) {
		t.Fatal(err)
	}
}

func TestOneChunkFailsAllOrNothing(t *testing.T) {
	var n atomic.Int32
	fb := &fakeBackend{fn: func(ctx context.Context, req classify.Request) (*classify.Response, error) {
		if n.Add(1) == 2 {
			return nil, errors.New("nope")
		}
		return scoreBy(func(int) float64 { return 1 })(ctx, req)
	}}
	res, err := newRR(fb, Options{}).Rank(context.Background(), "i", "p", mkItems(120))
	if !errors.Is(err, ErrClassifier) || len(res.Kept) != 0 {
		t.Fatal(err, res)
	}
}

func TestTimeoutNonCooperativeBackend(t *testing.T) {
	release := make(chan struct{})
	done := make(chan struct{})
	fb := &fakeBackend{fn: func(context.Context, classify.Request) (*classify.Response, error) {
		<-release
		close(done)
		return nil, errors.New("late")
	}}
	r := newRR(fb, Options{Budget: 80 * time.Millisecond})
	start := time.Now()
	_, err := r.Rank(context.Background(), "i", "p", mkItems(3))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 80*time.Millisecond+500*time.Millisecond {
		t.Fatalf("elapsed %v", el)
	}
	// Permit is still held by the stuck backend call.
	if len(sem) != 1 {
		t.Fatalf("sem=%d", len(sem))
	}
	close(release)
	<-done
	waitSemEmpty(t)
}

func waitSemEmpty(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(sem) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("semaphore leaked")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestParentCancelNotCounted(t *testing.T) {
	started := make(chan struct{}, 1)
	fb := &fakeBackend{fn: func(ctx context.Context, _ classify.Request) (*classify.Response, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r := newRR(fb, Options{Budget: 5 * time.Second})
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-started; cancel() }()
		if _, err := r.Rank(ctx, "i", "p", mkItems(3)); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	// Pre-cancelled parent.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Rank(ctx, "i", "p", mkItems(3)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	fb.fn = scoreBy(func(int) float64 { return 1 })
	if _, err := r.Rank(context.Background(), "i", "p", mkItems(3)); err != nil {
		t.Fatalf("breaker tripped by cancels: %v", err)
	}
	waitSemEmpty(t)
}

func TestBreaker(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	fb := &fakeBackend{fn: func(ctx context.Context, req classify.Request) (*classify.Response, error) {
		if fail.Load() {
			return nil, errors.New("x")
		}
		return scoreBy(func(int) float64 { return 1 })(ctx, req)
	}}
	now := time.Unix(1000, 0)
	r := newRR(fb, Options{})
	r.SetClock(func() time.Time { return now })
	rank := func() error { _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); return err }

	// Success in between resets the count.
	_ = rank()
	_ = rank()
	fail.Store(false)
	if err := rank(); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	_ = rank()
	_ = rank()
	if fb.calls() != 5 {
		t.Fatal(fb.calls())
	}
	if err := rank(); !errors.Is(err, ErrClassifier) {
		t.Fatal(err)
	}
	// Open now.
	before := fb.calls()
	if err := rank(); !errors.Is(err, ErrBreakerOpen) || fb.calls() != before {
		t.Fatal(err)
	}
	now = now.Add(59 * time.Second)
	if err := rank(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if err := rank(); !errors.Is(err, ErrClassifier) { // probe fails, re-opens at once
		t.Fatal(err)
	}
	if err := rank(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatal(err)
	}
	now = now.Add(61 * time.Second)
	fail.Store(false)
	if err := rank(); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	_ = rank()
	_ = rank()
	if err := rank(); !errors.Is(err, ErrClassifier) {
		t.Fatal(err) // fresh count after success: 3rd failure
	}
	if err := rank(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatal(err)
	}
}

func TestTimeoutsCountTowardBreaker(t *testing.T) {
	release := make(chan struct{})
	fb := &fakeBackend{fn: func(context.Context, classify.Request) (*classify.Response, error) {
		<-release
		return nil, errors.New("late")
	}}
	r := newRR(fb, Options{Budget: 20 * time.Millisecond})
	for i := 0; i < 3; i++ {
		if _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
	if _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); !errors.Is(err, ErrBreakerOpen) {
		t.Fatal(err)
	}
	close(release)
	waitSemEmpty(t)
}

func TestOpenOnceAndStickyFailure(t *testing.T) {
	var opens atomic.Int32
	r := New(func() (classify.Backend, error) {
		opens.Add(1)
		return nil, errors.New("op read failed: secret")
	}, "m", Options{Budget: time.Second})
	for i := 0; i < 3; i++ {
		_, err := r.Rank(context.Background(), "i", "p", mkItems(2))
		if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	}
	// Unavailability counts as a failure: the breaker is now open.
	if _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); !errors.Is(err, ErrBreakerOpen) {
		t.Fatal(err)
	}
	if opens.Load() != 1 {
		t.Fatalf("opens=%d", opens.Load())
	}
}

func TestSlowOpenFailureIsStickyAfterTimeout(t *testing.T) {
	release := make(chan struct{})
	var opens atomic.Int32
	r := New(func() (classify.Backend, error) {
		opens.Add(1)
		<-release
		return nil, errors.New("fail")
	}, "m", Options{Budget: 30 * time.Millisecond})
	if _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	close(release)
	<-r.ready
	if _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if opens.Load() != 1 {
		t.Fatal(opens.Load())
	}
}

func TestResultRacingDeadlineIsNotSuccess(t *testing.T) {
	// Backend ignores ctx and answers just after the budget; Rank must
	// never report success past the deadline.
	fb := &fakeBackend{fn: func(ctx context.Context, req classify.Request) (*classify.Response, error) {
		<-ctx.Done()
		return scoreBy(func(int) float64 { return 1 })(ctx, req)
	}}
	r := newRR(fb, Options{Budget: 20 * time.Millisecond})
	for i := 0; i < 2; i++ {
		if _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
	waitSemEmpty(t)
}

func TestBudgetAccessor(t *testing.T) {
	if New(nil, "m", Options{Budget: time.Second}).Budget() != time.Second || New(nil, "m", Options{}).Budget() != DefaultBudget {
		t.Fatal()
	}
}

func TestOpenNilBackendAndNilOpener(t *testing.T) {
	r := New(func() (classify.Backend, error) { return nil, nil }, "m", Options{})
	if _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	r = New(nil, "m", Options{})
	if _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	r = New(func() (classify.Backend, error) { panic("x") }, "m", Options{})
	if _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestSlowOpenExceedsBudgetThenReused(t *testing.T) {
	release := make(chan struct{})
	var opens atomic.Int32
	fb := &fakeBackend{fn: scoreBy(func(int) float64 { return 1 })}
	r := New(func() (classify.Backend, error) {
		opens.Add(1)
		<-release
		return fb, nil
	}, "m", Options{Budget: 50 * time.Millisecond})
	start := time.Now()
	if _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("slow")
	}
	if fb.calls() != 0 {
		t.Fatal("classify before open")
	}
	close(release)
	r.SetClock(time.Now)
	if _, err := r.Rank(context.Background(), "i", "p", mkItems(2)); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 1 {
		t.Fatal(opens.Load())
	}
}

func TestOpenIsLazy(t *testing.T) {
	var opens atomic.Int32
	r := New(func() (classify.Backend, error) { opens.Add(1); return nil, errors.New("x") }, "m", Options{})
	_, _ = r.Rank(context.Background(), "i", "p", mkItems(1)) // rejected before open
	if opens.Load() != 0 {
		t.Fatal("opened eagerly")
	}
}

func TestSemaphoreLimitsConcurrency(t *testing.T) {
	var cur, max atomic.Int32
	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	fb := &fakeBackend{fn: func(ctx context.Context, req classify.Request) (*classify.Response, error) {
		c := cur.Add(1)
		for {
			m := max.Load()
			if c <= m || max.CompareAndSwap(m, c) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		cur.Add(-1)
		return scoreBy(func(int) float64 { return 1 })(ctx, req)
	}}
	r := newRR(fb, Options{MaxCandidates: 500, Budget: 10 * time.Second})
	errc := make(chan error, 1)
	go func() {
		_, err := r.Rank(context.Background(), "i", "p", mkItems(400)) // 8 chunks
		errc <- err
	}()
	for i := 0; i < MaxConcurrent; i++ {
		<-entered
	}
	// No fifth may enter while four are blocked.
	select {
	case <-entered:
		t.Fatal("more than 4 in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if max.Load() != MaxConcurrent {
		t.Fatalf("max=%d", max.Load())
	}
	if fb.calls() != 8 {
		t.Fatal(fb.calls())
	}
}

func TestAdmissionWaitBoundedByBudget(t *testing.T) {
	// Fill the semaphore externally; Rank must time out waiting for admission.
	for i := 0; i < MaxConcurrent; i++ {
		sem <- struct{}{}
	}
	defer func() {
		for i := 0; i < MaxConcurrent; i++ {
			<-sem
		}
	}()
	fb := &fakeBackend{fn: scoreBy(func(int) float64 { return 1 })}
	r := newRR(fb, Options{Budget: 40 * time.Millisecond})
	if _, err := r.Rank(context.Background(), "i", "p", mkItems(3)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if fb.calls() != 0 {
		t.Fatal("called without permit")
	}
}

func TestConcurrentRanksRace(t *testing.T) {
	fb := &fakeBackend{fn: scoreBy(func(n int) float64 { return float64(n%10) / 10 })}
	r := newRR(fb, Options{Threshold: .5})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Rank(context.Background(), "i", "p", mkItems(60)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
