package ui

import (
	"context"
	"io"

	"github.com/samsaffron/term-llm/internal/llm"
)

// DefaultStreamBufferSize is the default buffer size for the event channel.
// Large enough to handle bursts while still providing backpressure.
const DefaultStreamBufferSize = 100

// StreamAdapter bridges an llm.Stream to a channel of StreamEvents.
// It handles event conversion and provides proper buffering with blocking sends
// to ensure no events are dropped.
type StreamAdapter struct {
	events chan StreamEvent
	stats  *SessionStats

	// ToolNameFilter is a tool name to skip (e.g., "ask_user" which has its own UI)
	ToolNameFilter string
}

// NewStreamAdapter creates a new StreamAdapter with the specified buffer size.
// If bufSize <= 0, DefaultStreamBufferSize is used.
func NewStreamAdapter(bufSize int) *StreamAdapter {
	if bufSize <= 0 {
		bufSize = DefaultStreamBufferSize
	}
	return &StreamAdapter{
		events: make(chan StreamEvent, bufSize),
		stats:  NewSessionStats(),
	}
}

// Events returns the channel to read events from.
func (a *StreamAdapter) Events() <-chan StreamEvent {
	return a.events
}

// EmitErrorAndClose sends an error event and closes the channel.
// Use this when stream creation fails before ProcessStream can be called.
func (a *StreamAdapter) EmitErrorAndClose(err error) {
	a.events <- ErrorEvent(err)
	close(a.events)
}

// Stats returns the session stats being tracked.
func (a *StreamAdapter) Stats() *SessionStats {
	return a.stats
}

// ProcessStream reads events from the llm.Stream and sends them to the events channel.
// This method blocks until the stream is exhausted or an error occurs.
// The events channel is closed when this method returns.
//
// Call this in a goroutine:
//
//	go adapter.ProcessStream(ctx, stream)
func (a *StreamAdapter) ProcessStream(ctx context.Context, stream llm.Stream) {
	defer close(a.events)

	var totalTokens int
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			a.events <- DoneEvent(totalTokens)
			return
		}
		if err != nil {
			// Check if context was cancelled
			if ctx.Err() != nil {
				a.events <- DoneEvent(totalTokens)
				return
			}
			a.events <- ErrorEvent(err)
			return
		}

		switch event.Type {
		case llm.EventError:
			if event.Err != nil {
				a.events <- ErrorEvent(event.Err)
				return
			}

		case llm.EventTextDelta:
			if event.Text != "" {
				a.events <- TextEvent(event.Text)
			}

		case llm.EventToolExecStart:
			a.stats.ToolStart()
			// Skip filtered tool (e.g., ask_user has its own UI)
			if a.ToolNameFilter != "" && event.ToolName == a.ToolNameFilter {
				continue
			}
			a.events <- ToolStartEvent(event.ToolCallID, event.ToolName, event.ToolInfo)

		case llm.EventToolExecEnd:
			a.stats.ToolEnd()
			// Skip filtered tool
			if a.ToolNameFilter != "" && event.ToolName == a.ToolNameFilter {
				continue
			}
			a.events <- ToolEndEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolSuccess)

		case llm.EventRetry:
			a.events <- RetryEvent(event.RetryAttempt, event.RetryMaxAttempts, event.RetryWaitSecs)

		case llm.EventUsage:
			if event.Use != nil {
				totalTokens = event.Use.OutputTokens
				a.stats.AddUsage(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens)
				a.events <- UsageEvent(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens)
			}

		case llm.EventPhase:
			if event.Text != "" {
				a.events <- PhaseEvent(event.Text)
			}
		}
	}
}
