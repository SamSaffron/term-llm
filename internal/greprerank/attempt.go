package greprerank

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errFeatureBudget is the cancellation cause of an expired operation budget.
// It never escapes the package: Finish reports context.DeadlineExceeded.
var errFeatureBudget = errors.New("greprerank: feature budget exhausted")

// Attempt is one rerank operation whose wall-clock budget covers more than
// Rank (for example streaming search, candidate building and rendering).
// Begin creates it without starting the clock; Start begins the budget; Finish
// ends it and updates the breaker exactly once.
type Attempt struct {
	r      *Reranker
	parent context.Context
	ctx    context.Context
	cancel context.CancelCauseFunc

	mu       sync.Mutex
	timer    *time.Timer
	started  bool
	finished bool
	result   error
}

// Begin starts an operation under parent. It fails immediately with the
// parent's error or ErrBreakerOpen, and does not start the budget clock.
func (r *Reranker) Begin(parent context.Context) (*Attempt, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if !r.allow() {
		return nil, ErrBreakerOpen
	}
	ctx, cancel := context.WithCancelCause(parent)
	return &Attempt{r: r, parent: parent, ctx: ctx, cancel: cancel}, nil
}

// Context returns the operation context, valid immediately. It is cancelled
// when the parent is, when the started budget expires, or by Finish.
func (a *Attempt) Context() context.Context { return a.ctx }

// Start starts the budget timer. Only the first call has an effect.
func (a *Attempt) Start() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started || a.finished {
		return
	}
	a.started = true
	a.timer = time.AfterFunc(a.r.opts.Budget, func() { a.cancel(errFeatureBudget) })
}

// Rank scores items within the operation context; it neither starts a timer
// nor touches the breaker. Pass its error to Finish.
func (a *Attempt) Rank(intent, pattern string, items []Item) (Result, error) {
	r := a.r
	if err := r.checkInput(intent, pattern, items); err != nil {
		return Result{}, err
	}
	if err := a.ctx.Err(); err != nil {
		return Result{}, err
	}
	scores, err := r.score(a.ctx, intent, pattern, items)
	if err != nil {
		return Result{}, err
	}
	result := rank(scores, r.opts.Threshold)
	// Results and deadline may race; an expired result is never a success.
	if err := a.ctx.Err(); err != nil {
		return Result{}, err
	}
	return result, nil
}

// Finish ends the operation, records the outcome with the breaker exactly
// once and returns the normalised error (nil on success). Later calls return
// the first result without recording again.
func (a *Attempt) Finish(err error) error {
	a.mu.Lock()
	if a.finished {
		res := a.result
		a.mu.Unlock()
		return res
	}
	a.finished = true
	if a.timer != nil {
		a.timer.Stop()
	}
	budget := errors.Is(context.Cause(a.ctx), errFeatureBudget)
	a.cancel(nil)
	a.result = a.classify(err, budget)
	res := a.result
	a.mu.Unlock()
	return res
}

func (a *Attempt) classify(err error, budget bool) error {
	r := a.r
	switch {
	case budget:
		// Our own budget was exhausted first (cause is set once, so a later
		// parent cancel cannot hide it, nor can this mask an earlier one).
		r.record(false)
		return context.DeadlineExceeded
	case errors.Is(err, ErrSkipped), errors.Is(err, ErrInput), errors.Is(err, ErrBreakerOpen):
		return err
	}
	// Real parent cancellation or deadline is neutral, even when a result
	// raced it (err == nil).
	if perr := a.parent.Err(); perr != nil {
		return perr
	}
	if err == nil {
		r.record(true)
		return nil
	}
	r.record(false)
	switch {
	case errors.Is(err, ErrUnavailable):
		return ErrUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}
	return ErrClassifier
}
