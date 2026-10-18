// Package stream provides import-cycle-free event stream, SSE decoding, and
// typed stream error helpers shared by provider implementations.
package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/restart"
)

// ErrClosed is returned by Sender.Send when the consumer closed the stream.
var ErrClosed = errors.New("stream closed")

// Sender provides safe event sending for stream producers.
// It handles context cancellation and closed-channel panics, preventing
// goroutine hangs when a consumer stops reading.
// Zero value is a no-op sender (for cases where events == nil).
type Sender struct {
	ctx context.Context
	ch  chan<- core.Event
}

// NewSender returns a Sender that delivers events to ch until ctx is done.
// A nil ch yields a no-op sender.
func NewSender(ctx context.Context, ch chan<- core.Event) Sender {
	return Sender{ctx: ctx, ch: ch}
}

// Context returns the context bounding sends.
func (s Sender) Context() context.Context { return s.ctx }

// Chan returns the underlying event channel (nil for a no-op sender).
func (s Sender) Chan() chan<- core.Event { return s.ch }

// Send sends an event, blocking until delivered, context cancelled, or channel closed.
// Returns nil on success, context error on cancellation, or a generic error on closed channel.
func (s Sender) Send(event core.Event) error {
	if s.ch == nil {
		return nil
	}
	if SafeSend(s.ctx, s.ch, event) {
		return nil
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return ErrClosed
}

// TrySend attempts a non-blocking send. Returns true if the event was delivered.
// Like Send, it recovers from closed-channel panics.
func (s Sender) TrySend(event core.Event) (sent bool) {
	if s.ch == nil {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			sent = false
		}
	}()
	select {
	case s.ch <- event:
		return true
	case <-s.ctx.Done():
		return false
	default:
		return false
	}
}

// SafeSend attempts to send an event to the channel, returning false if
// the channel is closed or context is cancelled. This prevents panics when
// the stream is closed while tool execution is still in progress.
func SafeSend(ctx context.Context, ch chan<- core.Event, event core.Event) (sent bool) {
	// Recover from panic if channel is closed
	defer func() {
		if r := recover(); r != nil {
			sent = false
		}
	}()

	select {
	case ch <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

type channelStream struct {
	ctx         context.Context
	cancel      context.CancelFunc
	cancelHook  func()
	closeOnce   sync.Once
	events      <-chan core.Event
	terminalErr <-chan error
	done        <-chan struct{}
}

// New starts run in a goroutine and returns a core.Stream that yields the
// events run sends. A non-nil error from run is delivered as a terminal
// core.EventError.
func New(ctx context.Context, run func(context.Context, Sender) error) core.Stream {
	return NewWithCancelHook(ctx, nil, run)
}

// NewWithCancelHook is like New, with an extra hook
// invoked by Close after canceling the stream context. Providers that create
// blocking resources before the stream goroutine (for example, an HTTP response
// body whose request used the parent context) can use the hook to unblock reads.
func NewWithCancelHook(ctx context.Context, cancelHook func(), run func(context.Context, Sender) error) core.Stream {
	if cancelHook != nil {
		cancelHook = sync.OnceFunc(cancelHook)
	}
	ctx, release, ownershipErr := restart.Child(ctx)
	streamCtx, cancel := context.WithCancel(ctx)
	ch := make(chan core.Event, 16)
	terminalErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		if release != nil {
			defer release()
		}
		defer close(done)
		defer func() {
			if r := recover(); r != nil && streamCtx.Err() == nil {
				err := fmt.Errorf("stream panic: %v", r)
				// Preserve the panic as a stream error for the consumer when possible.
				// Never let a provider/cleanup panic escape this goroutine: an
				// unrecovered panic in any goroutine terminates the whole TUI process.
				select {
				case ch <- core.Event{Type: core.EventError, Err: err}:
				default:
					terminalErr <- err
				}
			}
			close(terminalErr)
			close(ch)
		}()
		if ownershipErr != nil {
			if cancelHook != nil {
				cancelHook()
			}
			terminalErr <- ownershipErr
			return
		}
		sender := Sender{ctx: streamCtx, ch: ch}
		if err := run(streamCtx, sender); err != nil {
			// If the consumer has stopped draining and the buffer is full, preserve the
			// terminal error for Recv() rather than dropping it and reporting clean EOF.
			//
			// A cancelled stream context must not discard the error either. Recv
			// reports cancellation only while this goroutine is still running; once it
			// closes the channels, a dropped error becomes an indistinguishable clean
			// EOF and the caller reports a failed run as success. Keeping the error
			// makes both orderings terminal.
			select {
			case ch <- core.Event{Type: core.EventError, Err: err}:
			default:
				terminalErr <- err
			}
		}
	}()
	return &channelStream{ctx: streamCtx, cancel: cancel, cancelHook: cancelHook, events: ch, terminalErr: terminalErr, done: done}
}

func (s *channelStream) Recv() (core.Event, error) {
	// Non-blocking drain: consume any buffered event before checking ctx.Done().
	// This prevents dropping EventUsage/EventDone when ctx and events are both ready.
	select {
	case event, ok := <-s.events:
		if !ok {
			if err, ok := <-s.terminalErr; ok {
				return core.Event{Type: core.EventError, Err: err}, nil
			}
			return core.Event{}, io.EOF
		}
		return event, nil
	default:
	}

	select {
	case <-s.ctx.Done():
		return core.Event{}, s.ctx.Err()
	case event, ok := <-s.events:
		if !ok {
			if err, ok := <-s.terminalErr; ok {
				return core.Event{Type: core.EventError, Err: err}, nil
			}
			return core.Event{}, io.EOF
		}
		return event, nil
	}
}

func (s *channelStream) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.cancelHook != nil {
			s.cancelHook()
		}
	})
	<-s.done
	return nil
}
