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
)

// ErrReverseAdmissionFull rejects overload beyond the bounded waiting room.
var ErrReverseAdmissionFull = errors.New("reverse connection admission queue is full")

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
		return errors.New("reverse connection is closed")
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
		waitCtx, cancel := context.WithTimeout(ctx, ReverseAdmissionWait)
		defer cancel()
		ctx = waitCtx
		select {
		case active <- struct{}{}:
		case <-ctx.Done():
			return fmt.Errorf("reverse connection admission wait: %w", ctx.Err())
		case <-closed:
			return errors.New("reverse connection is closed")
		}
	}
	// A release can race cancellation, timeout, or tunnel teardown. Select may
	// choose the available slot even then; return it before admitting any work.
	if err := ctx.Err(); err != nil {
		<-active
		return err
	}
	select {
	case <-closed:
		<-active
		return errors.New("reverse connection is closed")
	default:
		return nil
	}
}
