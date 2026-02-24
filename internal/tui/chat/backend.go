package chat

import (
	"context"

	"github.com/samsaffron/term-llm/internal/ui"
)

// StreamBackend abstracts local LLM vs remote WebSocket.
type StreamBackend interface {
	SendMessage(ctx context.Context, text string) (<-chan ui.StreamEvent, error)
	Interrupt()
	Reset()
}
