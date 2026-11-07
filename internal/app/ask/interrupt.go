package ask

import (
	"context"
	"errors"
	"fmt"
)

// runInterruption reports whether the run was stopped by the caller (Ctrl-C or
// --timeout) rather than by a provider or tool failure. Checking the live
// context instead of errors.Is(err, context.Canceled) matters because runner
// cleanup joins the run context's cancellation onto the real error; that joined
// cancellation must not turn a provider failure into a silent success.
func runInterruption(ctx context.Context, err error) bool {
	if err == nil || ctx == nil {
		return false
	}
	return isCancellationError(ctx.Err())
}

func isCancellationError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// terminalRunError combines the display error with the runner's terminal error.
// A renderer that stops because the stream context was canceled must not hide a
// substantive provider failure; the provider diagnostic is authoritative, while
// two independent failures stay joined so neither is lost.
func terminalRunError(displayErr, streamErr error) error {
	if streamErr == nil || isCancellationError(streamErr) {
		return displayErr
	}
	if displayErr == nil || isCancellationError(displayErr) {
		return fmt.Errorf("streaming failed: %w", streamErr)
	}
	return errors.Join(displayErr, streamErr)
}
