package run

import (
	"context"
	"errors"
)

// IsCancellation reports whether err is a context cancellation or deadline.
func IsCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// JoinErrors keeps the terminal cause without losing typed classification. When
// a cleanup cancellation accompanies a substantive failure, the failure wins:
// joining context.Canceled onto a provider error makes callers classify the run
// as a user interruption and drop the real diagnostic.
func JoinErrors(existing, additional error) error {
	switch {
	case existing == nil:
		return additional
	case additional == nil:
		return existing
	case IsCancellation(existing) && !IsCancellation(additional):
		return additional
	case !IsCancellation(existing) && IsCancellation(additional):
		return existing
	default:
		return errors.Join(existing, additional)
	}
}
