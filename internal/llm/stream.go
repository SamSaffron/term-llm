package llm

import (
	"context"

	"github.com/samsaffron/term-llm/internal/llm/core/stream"
)

var errEventStreamClosed = stream.ErrClosed

// eventSender provides safe event sending for stream producers. Zero value is
// a no-op sender. It delegates to stream.Sender; fields stay private to this
// package for existing callers.
type eventSender struct {
	ctx context.Context
	ch  chan<- Event
}

func (s eventSender) inner() stream.Sender { return stream.NewSender(s.ctx, s.ch) }

// Send sends an event, blocking until delivered, context cancelled, or closed.
func (s eventSender) Send(event Event) error { return s.inner().Send(event) }

// TrySend attempts a non-blocking send and reports whether it was delivered.
func (s eventSender) TrySend(event Event) bool { return s.inner().TrySend(event) }

func newEventStream(ctx context.Context, run func(context.Context, eventSender) error) Stream {
	return stream.New(ctx, wrapRun(run))
}

// newEventStreamWithCancelHook is like newEventStream, with an extra hook
// invoked by Close after canceling the stream context.
func newEventStreamWithCancelHook(ctx context.Context, cancelHook func(), run func(context.Context, eventSender) error) Stream {
	return stream.NewWithCancelHook(ctx, cancelHook, wrapRun(run))
}

func wrapRun(run func(context.Context, eventSender) error) func(context.Context, stream.Sender) error {
	return func(ctx context.Context, s stream.Sender) error {
		return run(ctx, eventSender{ctx: s.Context(), ch: s.Chan()})
	}
}

func safeSendEvent(ctx context.Context, ch chan<- Event, event Event) bool {
	return stream.SafeSend(ctx, ch, event)
}
