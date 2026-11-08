package ask

import (
	"context"
	"errors"
	"fmt"
)

// errUserInterrupted is an explicit renderer-owned stop signal. Rich renderers
// handle Esc/Ctrl+C locally without cancelling the outer context; keeping that
// signal distinct from provider and cleanup cancellation lets the execution
// layer stop outstanding children and persist interrupted output without
// hiding a substantive provider failure.
var errUserInterrupted = errors.New("user interrupted")

// runInterruption reports whether the run was stopped by the caller (Ctrl-C or
// --timeout) rather than by a provider or tool failure. Checking the live
// context instead of errors.Is(err, context.Canceled) matters because runner
// cleanup joins the run context's cancellation onto the real error; that joined
// cancellation must not turn a provider failure into a silent success.
func runInterruption(ctx context.Context, err error) bool {
	if err == nil || ctx == nil {
		return false
	}
	return isCancellationError(ctx.Err()) || (errors.Is(err, errUserInterrupted) && isOnlyCancellation(err))
}

func isCancellationError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// terminalRunError combines the display error with the runner's terminal error.
// A renderer that stops because the stream context was canceled must not hide a
// substantive provider failure; the provider diagnostic is authoritative, while
// two independent failures stay joined so neither is lost.
func terminalRunError(displayErr, streamErr error) error {
	if streamErr == nil || isOnlyCancellation(streamErr) {
		return displayErr
	}
	if displayErr == nil || isOnlyCancellation(displayErr) {
		return fmt.Errorf("streaming failed: %w", streamErr)
	}
	return errors.Join(displayErr, streamErr)
}

// isOnlyCancellation reports whether err contains no substantive failure. It
// distinguishes a plain cancellation or joined cleanup cancellation from an
// error that also carries a provider/tool diagnostic.
func isOnlyCancellation(err error) bool {
	if err == nil {
		return true
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range multi.Unwrap() {
			if !isOnlyCancellation(child) {
				return false
			}
		}
		return true
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		if child := single.Unwrap(); child != nil {
			return isOnlyCancellation(child)
		}
	}
	return err == context.Canceled || err == context.DeadlineExceeded || errors.Is(err, errUserInterrupted)
}
