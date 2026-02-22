package ui

import (
	"context"
	"io"

	"github.com/samsaffron/term-llm/internal/edit"
	"github.com/samsaffron/term-llm/internal/llm"
)

// PlanStreamAdapter bridges an llm.Stream to a channel of StreamEvents,
// with inline edit marker parsing for the plan mode.
// It converts text stream events into inline edit events when markers are detected.
type PlanStreamAdapter struct {
	events chan StreamEvent
	stats  *SessionStats

	seenToolStarts map[string]struct{}
	seenToolEnds   map[string]struct{}

	parser *edit.InlineEditParser
}

// NewPlanStreamAdapter creates a new PlanStreamAdapter with the specified buffer size.
// If bufSize <= 0, DefaultStreamBufferSize is used.
func NewPlanStreamAdapter(bufSize int) *PlanStreamAdapter {
	if bufSize <= 0 {
		bufSize = DefaultStreamBufferSize
	}

	adapter := &PlanStreamAdapter{
		events:         make(chan StreamEvent, bufSize),
		stats:          NewSessionStats(),
		seenToolStarts: make(map[string]struct{}),
		seenToolEnds:   make(map[string]struct{}),
		parser:         edit.NewInlineEditParser(),
	}

	return adapter
}

// Events returns the channel to read events from.
func (a *PlanStreamAdapter) Events() <-chan StreamEvent {
	return a.events
}

// EmitErrorAndClose sends an error event and closes the channel.
// Use this when stream creation fails before ProcessStream can be called.
func (a *PlanStreamAdapter) EmitErrorAndClose(err error) {
	a.events <- ErrorEvent(err)
	close(a.events)
}

// Stats returns the session stats being tracked.
func (a *PlanStreamAdapter) Stats() *SessionStats {
	return a.stats
}

// ProcessStream reads events from the llm.Stream and sends them to the events channel.
// Text events are parsed for inline edit markers (INSERT/DELETE) and converted
// to StreamEventInlineInsert/StreamEventInlineDelete events.
// This method blocks until the stream is exhausted or an error occurs.
// The events channel is closed when this method returns.
//
// Call this in a goroutine:
//
//	go adapter.ProcessStream(ctx, stream)
func (a *PlanStreamAdapter) ProcessStream(ctx context.Context, stream llm.Stream) {
	defer close(a.events)

	// Set up parser callbacks
	a.parser.OnEdit = func(e edit.InlineEdit) {
		switch e.Type {
		case edit.InlineEditInsert:
			a.events <- InlineInsertEvent(e.After, e.Content)
		case edit.InlineEditDelete:
			a.events <- InlineDeleteEvent(e.From, e.To)
		}
	}
	a.parser.OnText = func(text string) {
		// Emit non-marker text as regular text events
		if text != "" {
			a.events <- TextEvent(text)
		}
	}
	a.parser.OnPartialInsert = func(after string, line string) {
		// Stream each line as it arrives during INSERT
		a.events <- PartialInsertEvent(after, line)
	}

	var totalTokens int
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			// Flush any remaining content in the parser
			a.parser.Flush()
			a.events <- DoneEvent(totalTokens)
			return
		}
		if err != nil {
			// Check if context was cancelled
			if ctx.Err() != nil {
				a.parser.Flush()
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
				// Feed text through the inline edit parser
				a.parser.Feed(event.Text)
			}

		case llm.EventToolExecStart:
			if event.ToolCallID != "" {
				if _, ok := a.seenToolStarts[event.ToolCallID]; ok {
					continue
				}
				a.seenToolStarts[event.ToolCallID] = struct{}{}
			}
			a.stats.ToolStart()
			a.events <- ToolStartEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolArgs)

		case llm.EventToolExecEnd:
			if event.ToolCallID != "" {
				if _, ok := a.seenToolEnds[event.ToolCallID]; ok {
					continue
				}
				a.seenToolEnds[event.ToolCallID] = struct{}{}
			}
			a.stats.ToolEnd()
			a.events <- ToolEndEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolSuccess)

		case llm.EventRetry:
			a.events <- RetryEvent(event.RetryAttempt, event.RetryMaxAttempts, event.RetryWaitSecs)

		case llm.EventUsage:
			if event.Use != nil {
				totalTokens = event.Use.OutputTokens
				a.stats.AddUsage(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens, event.Use.CacheWriteTokens)
				a.events <- UsageEvent(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens, event.Use.CacheWriteTokens)
			}

		case llm.EventPhase:
			if event.Text != "" {
				a.events <- PhaseEvent(event.Text)
			}
		}
	}
}
