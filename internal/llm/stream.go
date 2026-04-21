package llm

import (
	"context"
	"fmt"
	"io"
)

// eventSender provides safe event sending for stream producers.
// It handles context cancellation and closed-channel panics, preventing
// goroutine hangs when a consumer stops reading.
// Zero value is a no-op sender (for cases where events == nil).
type eventSender struct {
	ctx context.Context
	ch  chan<- Event
}

// Send sends an event, blocking until delivered, context cancelled, or channel closed.
// Returns nil on success, context error on cancellation, or a generic error on closed channel.
func (s eventSender) Send(event Event) error {
	if s.ch == nil {
		return nil
	}
	if safeSendEvent(s.ctx, s.ch, event) {
		return nil
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("stream closed")
}

// TrySend attempts a non-blocking send. Returns true if the event was delivered.
// Like Send, it recovers from closed-channel panics.
func (s eventSender) TrySend(event Event) (sent bool) {
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

// safeSendEvent attempts to send an event to the channel, returning false if
// the channel is closed or context is cancelled. This prevents panics when
// the stream is closed while tool execution is still in progress.
func safeSendEvent(ctx context.Context, ch chan<- Event, event Event) (sent bool) {
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
	events      <-chan Event
	terminalErr <-chan error
	done        <-chan struct{}
}

func newEventStream(ctx context.Context, run func(context.Context, eventSender) error) Stream {
	streamCtx, cancel := context.WithCancel(ctx)
	ch := make(chan Event, 16)
	terminalErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sender := eventSender{ctx: streamCtx, ch: ch}
		if err := run(streamCtx, sender); err != nil {
			// If the consumer has stopped draining and the buffer is full, preserve the
			// terminal error for Recv() rather than dropping it and reporting clean EOF.
			select {
			case ch <- Event{Type: EventError, Err: err}:
			default:
				terminalErr <- err
			}
		}
		close(terminalErr)
		close(ch)
	}()
	return &channelStream{ctx: streamCtx, cancel: cancel, events: ch, terminalErr: terminalErr, done: done}
}

func (s *channelStream) Recv() (Event, error) {
	// Non-blocking drain: consume any buffered event before checking ctx.Done().
	// This prevents dropping EventUsage/EventDone when ctx and events are both ready.
	select {
	case event, ok := <-s.events:
		if !ok {
			if err, ok := <-s.terminalErr; ok {
				return Event{Type: EventError, Err: err}, nil
			}
			return Event{}, io.EOF
		}
		return event, nil
	default:
	}

	select {
	case <-s.ctx.Done():
		return Event{}, s.ctx.Err()
	case event, ok := <-s.events:
		if !ok {
			if err, ok := <-s.terminalErr; ok {
				return Event{Type: EventError, Err: err}, nil
			}
			return Event{}, io.EOF
		}
		return event, nil
	}
}

func (s *channelStream) Close() error {
	s.cancel()
	<-s.done
	return nil
}
