package ui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/diff"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
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
			a.events <- ToolStartEvent(event.ToolCallID, event.ToolName, event.ToolInfo)

		case llm.EventToolExecEnd:
			a.stats.ToolEnd()
			a.events <- ToolEndEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolSuccess)

			// Parse image markers from tool output and emit image events
			if event.ToolOutput != "" {
				for _, imagePath := range parseImageMarkers(event.ToolOutput) {
					a.events <- ImageEvent(imagePath)
				}
				// Parse diff markers from edit_file tool output and emit diff events
				if event.ToolName == tools.EditFileToolName {
					for _, d := range parseDiffMarkers(event.ToolOutput) {
						a.events <- DiffEvent(d.File, d.Old, d.New)
					}
				}
			}

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

// parseImageMarkers extracts image paths from tool output that contain __IMAGE__: markers
func parseImageMarkers(output string) []string {
	var images []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "__IMAGE__:") {
			path := strings.TrimPrefix(line, "__IMAGE__:")
			path = strings.TrimSpace(path)
			if path != "" {
				images = append(images, path)
			}
		}
	}
	return images
}

// diffData represents the JSON structure in __DIFF__: markers
type diffData struct {
	File string `json:"f"`
	Old  string `json:"o"`
	New  string `json:"n"`
}

// parseDiffMarkers extracts diff data from tool output that contain __DIFF__: markers
// Format: __DIFF__:<base64-encoded JSON>
func parseDiffMarkers(output string) []diffData {
	var diffs []diffData
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
			var d diffData
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
