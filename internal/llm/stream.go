package llm

import (
	"context"
	"io"
)

type channelStream struct {
	ctx         context.Context
	cancel      context.CancelFunc
	events      <-chan Event
	terminalErr <-chan error
	done        <-chan struct{}
}

func newEventStream(ctx context.Context, run func(context.Context, chan<- Event) error) Stream {
	streamCtx, cancel := context.WithCancel(ctx)
	ch := make(chan Event, 16)
	terminalErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := run(streamCtx, ch); err != nil {
			// If the consumer has stopped draining and the buffer is full, preserve the
			// terminal error for Recv() rather than dropping it and reporting clean EOF.
			select {
			case ch <- Event{Type: EventError, Err: err}:
			case <-streamCtx.Done():
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
