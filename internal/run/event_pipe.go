package run

import (
	"context"
	"io"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// EventPipe adapts a Runner EventSink into an llm.Stream. It is useful for
// platforms that already consume streams through existing stream adapters while
// the shared runner owns execution.
type EventPipe struct {
	ctx       context.Context
	events    chan core.Event
	err       error
	closeOnce sync.Once
}

// NewEventPipe creates an EventPipe with the requested event buffer size.
func NewEventPipe(ctx context.Context, buffer int) *EventPipe {
	if ctx == nil {
		ctx = context.Background()
	}
	if buffer < 0 {
		buffer = 0
	}
	return &EventPipe{
		ctx:    ctx,
		events: make(chan core.Event, buffer),
	}
}

// Events exposes the raw event channel for consumers that already multiplex on
// channels. Prefer Recv when an llm.Stream is accepted.
func (p *EventPipe) Events() <-chan core.Event {
	if p == nil {
		return nil
	}
	return p.events
}

// Event implements EventSink. It drops the event only when the pipe context has
// been cancelled.
func (p *EventPipe) Event(ev core.Event) {
	_ = p.EventWithError(ev)
}

// EventWithError implements ErrorEventSink so runner producers can stop when a
// consumer has cancelled the pipe context.
func (p *EventPipe) EventWithError(ev core.Event) error {
	if p == nil {
		return nil
	}
	select {
	case p.events <- ev:
		return nil
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

// CloseWithError marks the producer complete. It must be called exactly once by
// the producer after Runner.Run returns.
func (p *EventPipe) CloseWithError(err error) {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		p.err = err
		close(p.events)
	})
}

// Recv implements llm.Stream.
func (p *EventPipe) Recv() (core.Event, error) {
	if p == nil {
		return core.Event{}, io.EOF
	}
	handleClosed := func() (core.Event, error) {
		if p.err != nil {
			return core.Event{}, p.err
		}
		return core.Event{}, io.EOF
	}

	// Prefer already-buffered or terminal events over context cancellation so
	// callers that cancel after an interrupt can still drain in-flight output and
	// see the runner's real terminal error.
	select {
	case ev, ok := <-p.events:
		if !ok {
			return handleClosed()
		}
		return ev, nil
	default:
	}

	select {
	case ev, ok := <-p.events:
		if !ok {
			return handleClosed()
		}
		return ev, nil
	case <-p.ctx.Done():
		return core.Event{}, p.ctx.Err()
	}
}

// Close implements llm.Stream. Closing is producer-owned via CloseWithError, so
// this is intentionally a no-op for stream adapters.
func (p *EventPipe) Close() error { return nil }
