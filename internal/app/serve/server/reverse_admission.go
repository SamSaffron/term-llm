package server

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Reverse admission bounds include streams whose terminal frame has arrived but
// whose body is still undrained. A bounded waiting room absorbs short browser
// bursts without increasing active operations or allocating their body buffers.
const (
	ReverseMaxActive  = 16
	ReverseMaxWaiting = 64
	// ReverseAdmissionWait allows short requests to drain in waves, but bounds
	// waiting behind long streams or a stopped peer.
	ReverseAdmissionWait = 2 * time.Second
	// ReverseMaxStreams bounds long-lived event streams that leave the request
	// pool once the node identifies them, so open tabs cannot pin the slots
	// page loads and API calls need. A full stream pool falls back to holding a
	// request slot, as before.
	ReverseMaxStreams = 32
	// ReverseStreamBodyBytes is a pooled event stream's buffered-body window.
	// Event frames are small, so the stream pool's worst-case buffering stays
	// a fraction of the request pool's (32 x 2 MiB versus 16 x 8 MiB).
	ReverseStreamBodyBytes = 2 << 20
)

var (
	// ErrReverseBusy marks admission failures caused by tunnel capacity rather
	// than an unreachable node; callers may retry shortly.
	ErrReverseBusy = errors.New("reverse connection is at capacity")
	// ErrReverseAdmissionFull rejects overload beyond the bounded waiting room.
	ErrReverseAdmissionFull = fmt.Errorf("%w: admission queue is full", ErrReverseBusy)
	// ErrReverseAdmissionTimeout reports that no request slot freed in time. It
	// is distinct from the caller's own cancellation or deadline.
	ErrReverseAdmissionTimeout = fmt.Errorf("%w: no request slot freed within %v", ErrReverseBusy, ReverseAdmissionWait)

	errReverseClosed = errors.New("reverse connection is closed")
)

// AdmitReverseRequest acquires an active slot, waiting only when necessary.
// The caller supplies per-connection channels sized by the reverse bounds and
// must release the active slot when the operation/body finishes. Waiting uses
// the caller's goroutine, with no admission workers or body/pending allocations.
func AdmitReverseRequest(ctx context.Context, active, waiting chan struct{}, closed <-chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-closed:
		return errReverseClosed
	default:
	}
	select {
	case active <- struct{}{}:
	default:
		select {
		case waiting <- struct{}{}:
		default:
			return ErrReverseAdmissionFull
		}
		defer func() { <-waiting }()
		timer := time.NewTimer(ReverseAdmissionWait)
		defer timer.Stop()
		select {
		case active <- struct{}{}:
		case <-ctx.Done():
			return fmt.Errorf("reverse connection admission wait: %w", ctx.Err())
		case <-timer.C:
			return ErrReverseAdmissionTimeout
		case <-closed:
			return errReverseClosed
		}
	}
	// A release can race cancellation or tunnel teardown. Select may choose the
	// available slot even then; return it before admitting any work.
	if err := ctx.Err(); err != nil {
		<-active
		return err
	}
	select {
	case <-closed:
		<-active
		return errReverseClosed
	default:
		return nil
	}
}
