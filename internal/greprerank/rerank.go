package greprerank

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
)

// Defaults applied when Options leaves a field at zero.
const (
	DefaultBudget        = 2 * time.Second
	DefaultMaxCandidates = 150

	// ChunkSize is the maximum number of results per classify request.
	ChunkSize = 50
	// MaxConcurrent is the process-wide cap on in-flight classify calls.
	MaxConcurrent = 4
	// MaxPromptBytes bounds the intent and pattern sent to the backend.
	MaxPromptBytes = 4 << 10

	// FallbackTopN is how many matches are kept when none reach the threshold.
	FallbackTopN = 3

	breakerFailures = 3
	breakerCooldown = 60 * time.Second
)

// Sentinel errors. Rank never returns backend error text.
var (
	// ErrUnavailable means the backend could not be opened (sticky).
	ErrUnavailable = errors.New("greprerank: classifier unavailable")
	// ErrBreakerOpen means recent repeated failures disabled reranking.
	ErrBreakerOpen = errors.New("greprerank: temporarily disabled after repeated failures")
	// ErrClassifier means the backend failed or returned an invalid response.
	ErrClassifier = errors.New("greprerank: classifier error")
	// ErrSkipped means there was nothing to rank (fewer than two items or an
	// empty intent). It is neutral for the breaker and not a failure; callers
	// should omit any classifier note for it.
	ErrSkipped = errors.New("greprerank: nothing to rank")
	// ErrInput means the request was invalid (too many candidates or an
	// oversized intent/pattern) and was rejected before any backend call. It
	// is neutral for the breaker but callers should report a classifier error.
	ErrInput = errors.New("greprerank: invalid input")
)

// Options configure a Reranker.
type Options struct {
	Threshold     float64
	Budget        time.Duration
	MaxCandidates int
}

// Item is one candidate: a path relative to the search root and its match
// block. Lines of Block starting with "> " are matched lines.
type Item struct {
	Path  string
	Block string
}

// Result lists kept items by index into the input, in rank order.
type Result struct {
	Kept   []int
	Scores []float64 // parallel to Kept
	Culled int       // items not kept
	// BelowThresholdAll is set when no item reached the threshold and Kept
	// holds the top FallbackTopN items instead.
	BelowThresholdAll bool
}

// sem is the process-wide classify admission semaphore.
var sem = make(chan struct{}, MaxConcurrent)

// Reranker reranks grep candidates through a lazily opened backend.
type Reranker struct {
	open  func() (classify.Backend, error)
	model string
	opts  Options

	once    sync.Once
	ready   chan struct{}
	backend classify.Backend
	openErr error

	mu        sync.Mutex
	now       func() time.Time
	fails     int
	openUntil time.Time
}

// New returns a Reranker. open is not called until the first Rank.
func New(open func() (classify.Backend, error), model string, opts Options) *Reranker {
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget
	}
	if opts.MaxCandidates <= 0 {
		opts.MaxCandidates = DefaultMaxCandidates
	}
	return &Reranker{open: open, model: model, opts: opts, ready: make(chan struct{}), now: time.Now}
}

// MaxCandidates returns the candidate cap.
func (r *Reranker) MaxCandidates() int { return r.opts.MaxCandidates }

// Budget returns the wall-clock budget for one Rank call.
func (r *Reranker) Budget() time.Duration { return r.opts.Budget }

// Threshold returns the relevance threshold.
func (r *Reranker) Threshold() float64 { return r.opts.Threshold }

// SetClock replaces the breaker clock (for tests).
func (r *Reranker) SetClock(now func() time.Time) {
	r.mu.Lock()
	r.now = now
	r.mu.Unlock()
}

// Rank scores items against intent in one operation: Begin, Start,
// Attempt.Rank and Finish. Errors are ErrSkipped, ErrInput, ErrUnavailable,
// ErrBreakerOpen, ErrClassifier, context.DeadlineExceeded (budget exhausted)
// or the parent context's error (not counted by the breaker).
func (r *Reranker) Rank(ctx context.Context, intent, pattern string, items []Item) (Result, error) {
	if err := r.checkInput(intent, pattern, items); err != nil {
		return Result{}, err
	}
	a, err := r.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	a.Start()
	res, err := a.Rank(intent, pattern, items)
	if err = a.Finish(err); err != nil {
		return Result{}, err
	}
	return res, nil
}

func (r *Reranker) checkInput(intent, pattern string, items []Item) error {
	if len(items) < 2 || intent == "" {
		return ErrSkipped
	}
	if len(items) > r.opts.MaxCandidates || len(intent) > MaxPromptBytes || len(pattern) > MaxPromptBytes {
		return ErrInput
	}
	return nil
}

func (r *Reranker) score(ctx context.Context, intent, pattern string, items []Item) ([]float64, error) {
	backend, err := r.ensureOpen(ctx)
	if err != nil {
		return nil, err
	}
	reqs, err := buildRequests(ctx, r.model, intent, pattern, items)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrClassifier
	}
	type chunkResult struct {
		start  int
		scores []float64
		err    error
	}
	out := make(chan chunkResult, len(reqs))
	for i := range reqs {
		go func(c chunkRequest) {
			res := chunkResult{start: c.start}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				// Both select arms can be ready after expiry; never admit a
				// new backend call merely because a permit also became free.
				if res.err = ctx.Err(); res.err == nil {
					res.scores, res.err = callBackend(ctx, backend, c)
				}
			case <-ctx.Done():
				res.err = ctx.Err()
			}
			out <- res
		}(reqs[i])
	}
	scores := make([]float64, len(items))
	for range reqs {
		select {
		case res := <-out:
			if res.err != nil {
				return nil, res.err
			}
			copy(scores[res.start:], res.scores)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return scores, nil
}

func callBackend(ctx context.Context, b classify.Backend, c chunkRequest) (scores []float64, err error) {
	defer func() {
		if p := recover(); p != nil {
			scores, err = nil, fmt.Errorf("backend panic")
		}
	}()
	resp, err := b.Classify(ctx, c.req)
	if err != nil {
		return nil, err
	}
	return validate(resp, c)
}

func validate(resp *classify.Response, c chunkRequest) ([]float64, error) {
	if resp == nil || len(resp.Answers) != c.count {
		return nil, errors.New("malformed response")
	}
	scores := make([]float64, c.count)
	for k := 0; k < c.count; k++ {
		a, ok := resp.Answers[questionID(c.start+k+1)]
		if !ok || a.Type != classify.TypeNoul || a.Noul == nil {
			return nil, errors.New("malformed response")
		}
		v := *a.Noul
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return nil, errors.New("malformed response")
		}
		scores[k] = v
	}
	return scores, nil
}

// ensureOpen opens the backend at most once per Reranker; the wait is bounded
// by ctx while the open itself continues in its own goroutine.
func (r *Reranker) ensureOpen(ctx context.Context) (classify.Backend, error) {
	r.once.Do(func() {
		go func() {
			defer close(r.ready)
			defer func() {
				if p := recover(); p != nil {
					r.backend, r.openErr = nil, errors.New("open panic")
				}
			}()
			if r.open == nil {
				r.openErr = errors.New("no opener")
				return
			}
			r.backend, r.openErr = r.open()
		}()
	})
	select {
	case <-r.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if r.openErr != nil || r.backend == nil {
		return nil, ErrUnavailable
	}
	return r.backend, nil
}

func (r *Reranker) allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.openUntil.IsZero() {
		return true
	}
	if r.now().Before(r.openUntil) {
		return false
	}
	// Cooldown elapsed: allow probes; one more failure re-opens.
	r.openUntil = time.Time{}
	r.fails = breakerFailures - 1
	return true
}

func (r *Reranker) record(ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ok {
		r.fails = 0
		r.openUntil = time.Time{}
		return
	}
	r.fails++
	if r.fails >= breakerFailures {
		r.openUntil = r.now().Add(breakerCooldown)
	}
}

// rank orders by score desc (stable) and applies the threshold.
func rank(scores []float64, threshold float64) Result {
	idx := make([]int, len(scores))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
	var res Result
	for _, i := range idx {
		if scores[i] >= threshold {
			res.Kept = append(res.Kept, i)
		}
	}
	if len(res.Kept) == 0 {
		res.BelowThresholdAll = true
		n := FallbackTopN
		if n > len(idx) {
			n = len(idx)
		}
		res.Kept = append(res.Kept, idx[:n]...)
	}
	res.Scores = make([]float64, len(res.Kept))
	for k, i := range res.Kept {
		res.Scores[k] = scores[i]
	}
	res.Culled = len(scores) - len(res.Kept)
	return res
}
