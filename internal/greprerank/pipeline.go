package greprerank

import (
	"context"
	"errors"
	"sync"
)

// MaxSearchWorkers bounds all potentially detached intent search/pipeline
// workers process-wide, separately from the classify-call semaphore.
const MaxSearchWorkers = 4

// ErrBusy means non-blocking search admission found all workers occupied.
// It is a neutral skip, not a classifier failure.
var ErrBusy = errors.New("greprerank: intent search workers busy")

var searchWorkers = make(chan struct{}, MaxSearchWorkers)

// RunAttempt retains the ordinary prefix while expanding the search once.
// The clock starts at the first prefix (ordinary cap), or at search completion.
// Only the ordinary formatter runs on the caller; all added work, including
// request construction and ranked rendering, is selected against the deadline.
// ErrBusy means no worker was started: the caller must run ordinary search.
func RunAttempt[T any](parent context.Context, attempt *Attempt,
	search func(context.Context, func(T)) (T, error),
	finish func(context.Context, T) (string, error), ordinary func(T) string,
) (string, error) {
	if err := parent.Err(); err != nil {
		return "", attempt.Finish(err)
	}
	if err := attempt.Context().Err(); err != nil {
		return "", attempt.Finish(err)
	}
	select {
	case searchWorkers <- struct{}{}:
	default:
		return "", attempt.Finish(ErrBusy)
	}
	prefix := make(chan T, 1)
	type outcome struct {
		text string
		err  error
	}
	result := make(chan outcome, 1)
	var once sync.Once
	publish := func(value T) { once.Do(func() { attempt.Start(); prefix <- value }) }
	go func() {
		// A caller timeout must not release admission for a still-blocked search.
		defer func() { <-searchWorkers }()
		value, err := search(attempt.Context(), publish)
		publish(value)
		text := ""
		if err == nil && attempt.Context().Err() == nil {
			text, err = finish(attempt.Context(), value)
		}
		if err == nil {
			err = attempt.Context().Err()
		}
		result <- outcome{text, err}
	}()
	var value T
	select {
	case value = <-prefix:
	case <-parent.Done():
		return "", attempt.Finish(parent.Err())
	}
	// Only the ordinary prefix is formatted on failure. That is baseline grep
	// work; expanded/ranked rendering runs exclusively in the timed worker.
	select {
	case got := <-result:
		err := attempt.Finish(got.err)
		if err != nil {
			return ordinary(value), err
		}
		return got.text, nil
	case <-attempt.Context().Done():
		err := attempt.Finish(attempt.Context().Err())
		return ordinary(value), err
	}
}
