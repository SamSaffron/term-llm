package chat

import (
	"context"
	"errors"

	"github.com/samsaffron/term-llm/internal/ui"
)

// LocalBackend wraps the existing local streaming logic.
type LocalBackend struct {
	sendFunc      func(ctx context.Context, text string) (<-chan ui.StreamEvent, error)
	interruptFunc func()
	resetFunc     func()
}

// NewLocalBackend creates a LocalBackend using the provided callbacks.
func NewLocalBackend(send func(ctx context.Context, text string) (<-chan ui.StreamEvent, error), interrupt func(), reset func()) *LocalBackend {
	return &LocalBackend{
		sendFunc:      send,
		interruptFunc: interrupt,
		resetFunc:     reset,
	}
}

// SendMessage sends a message through the local engine and returns stream events.
func (b *LocalBackend) SendMessage(ctx context.Context, text string) (<-chan ui.StreamEvent, error) {
	if b == nil || b.sendFunc == nil {
		return nil, errors.New("local backend not configured")
	}
	return b.sendFunc(ctx, text)
}

// Interrupt cancels the current stream.
func (b *LocalBackend) Interrupt() {
	if b == nil || b.interruptFunc == nil {
		return
	}
	b.interruptFunc()
}

// Reset clears the conversation state.
func (b *LocalBackend) Reset() {
	if b == nil || b.resetFunc == nil {
		return
	}
	b.resetFunc()
}
