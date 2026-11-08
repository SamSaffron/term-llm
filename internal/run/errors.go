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
	case isPureCancellation(existing) && !isPureCancellation(additional):
		return additional
	case !isPureCancellation(existing) && isPureCancellation(additional):
		return existing
	default:
		return errors.Join(existing, additional)
	}
}

// isPureCancellation recurses through joined and wrapped errors before deciding
// that an error is only cancellation. A joined provider failure plus cleanup
// cancellation is substantive and must not be mistaken for a plain stop.
func isPureCancellation(err error) bool {
	if err == nil {
		return true
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range multi.Unwrap() {
			if !isPureCancellation(child) {
				return false
			}
		}
		return true
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		if child := single.Unwrap(); child != nil {
			return isPureCancellation(child)
		}
	}
	return IsCancellation(err)
}
