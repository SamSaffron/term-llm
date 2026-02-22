package ui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/diff"
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

	seenToolStarts map[string]struct{}
	seenToolEnds   map[string]struct{}
}

// NewStreamAdapter creates a new StreamAdapter with the specified buffer size.
// If bufSize <= 0, DefaultStreamBufferSize is used.
func NewStreamAdapter(bufSize int) *StreamAdapter {
	if bufSize <= 0 {
		bufSize = DefaultStreamBufferSize
	}
	return &StreamAdapter{
		events:         make(chan StreamEvent, bufSize),
		stats:          NewSessionStats(),
		seenToolStarts: make(map[string]struct{}),
		seenToolEnds:   make(map[string]struct{}),
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

		case llm.EventToolCall:
			// Tool call announced during streaming - preserves interleaving order
			if event.Tool != nil {
				toolCallID := event.ToolCallID
				if toolCallID == "" {
					toolCallID = event.Tool.ID
				}
				// Skip if already seen (prevents double-counting with EventToolExecStart).
				// If toolCallID is empty, don't dedupe - treat each call as unique.
				if toolCallID != "" {
					if _, ok := a.seenToolStarts[toolCallID]; ok {
						continue
					}
					a.seenToolStarts[toolCallID] = struct{}{}
				}
				toolInfo := event.ToolInfo
				if toolInfo == "" {
					toolInfo = llm.ExtractToolInfo(*event.Tool)
				}
				toolArgs := event.ToolArgs
				if len(toolArgs) == 0 {
					toolArgs = event.Tool.Arguments
				}
				a.stats.ToolStart()
				a.events <- ToolStartEvent(toolCallID, event.Tool.Name, toolInfo, toolArgs)
			}

		case llm.EventToolExecStart:
			// Skip if already seen. If toolCallID is empty, don't dedupe - treat as unique.
			if event.ToolCallID != "" {
				if _, ok := a.seenToolStarts[event.ToolCallID]; ok {
					continue
				}
				a.seenToolStarts[event.ToolCallID] = struct{}{}
			}
			a.stats.ToolStart()
			a.events <- ToolStartEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolArgs)

		case llm.EventToolExecEnd:
			// Skip if already seen. If toolCallID is empty, don't dedupe - treat as unique.
			if event.ToolCallID != "" {
				if _, ok := a.seenToolEnds[event.ToolCallID]; ok {
					continue
				}
				a.seenToolEnds[event.ToolCallID] = struct{}{}
			}
			a.stats.ToolEnd()
			a.events <- ToolEndEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolSuccess)

			// Emit image events from structured data
			for _, imagePath := range event.ToolImages {
				a.events <- ImageEvent(imagePath)
			}
			// Emit diff events from structured data
			for _, d := range event.ToolDiffs {
				a.events <- DiffEvent(d.File, d.Old, d.New, d.Line)
			}

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

		case llm.EventInterjection:
			if event.Text != "" {
				a.events <- InterjectionEvent(event.Text)
			}
		}
	}
}

// DiffData is an alias for llm.DiffData for backward compatibility.
type DiffData = llm.DiffData

// ParseDiffMarkers extracts diff data from tool output that contain __DIFF__: markers.
// Used for backward compatibility when rendering old sessions that have markers in Display/Content.
// Format: __DIFF__:<base64-encoded JSON>
func ParseDiffMarkers(output string) []DiffData {
	var diffs []DiffData
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "__DIFF__:") {
			encoded := strings.TrimPrefix(line, "__DIFF__:")
			encoded = strings.TrimSpace(encoded)
			if encoded == "" {
				continue
			}
			// Check decoded size before allocating (prevent large buffer allocation)
			decodedLen := base64.StdEncoding.DecodedLen(len(encoded))
			if decodedLen > diff.MaxDiffSize {
				continue
			}
			// Decode base64
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				continue
			}
			// Parse JSON
			var d DiffData
			if err := json.Unmarshal(decoded, &d); err != nil {
				continue
			}
			if d.File != "" {
				diffs = append(diffs, d)
			}
		}
	}
	return diffs
}
