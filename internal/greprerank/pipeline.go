package greprerank

import (
	"context"
	"sync"
)

// RunAttempt retains the ordinary prefix while expanding the search once.
// The clock starts at the first prefix (ordinary cap), or at search completion.
// Only the ordinary formatter runs on the caller; all added work, including
// request construction and ranked rendering, is selected against the deadline.
func RunAttempt[T any](parent context.Context, attempt *Attempt,
	search func(context.Context, func(T)) (T, error),
	finish func(context.Context, T) (string, error), ordinary func(T) string,
) (string, error) {
	prefix := make(chan T, 1)
	type outcome struct {
		text string
		err  error
	}
	result := make(chan outcome, 1)
	var once sync.Once
	publish := func(value T) { once.Do(func() { attempt.Start(); prefix <- value }) }
	go func() {
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
