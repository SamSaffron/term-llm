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

	attemptInput          int
	attemptOutput         int
	attemptCached         int
	attemptCacheWrite     int
	attemptUsageCalls     int
	attemptUsageCommitted bool
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

func (a *StreamAdapter) markAttemptCommitted() {
	a.attemptInput, a.attemptOutput, a.attemptCached, a.attemptCacheWrite, a.attemptUsageCalls = 0, 0, 0, 0, 0
	a.attemptUsageCommitted = true
}

func (a *StreamAdapter) resetAttemptUsage() {
	a.attemptInput, a.attemptOutput, a.attemptCached, a.attemptCacheWrite, a.attemptUsageCalls = 0, 0, 0, 0, 0
	a.attemptUsageCommitted = false
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
	if ctx == nil {
		ctx = context.Background()
	}
	defer close(a.events)

	emit := func(event StreamEvent) bool {
		if ctx.Err() != nil {
			return false
		}
		select {
		case a.events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}

	var totalTokens int
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		event, err := stream.Recv()
		if err == io.EOF {
			emit(DoneEvent(totalTokens))
			return
		}
		if err != nil {
			// Check if context was cancelled
			if ctx.Err() != nil {
				return
			}
			emit(ErrorEvent(err))
			return
		}

		switch event.Type {
		case llm.EventError:
			if event.Err != nil {
				emit(ErrorEvent(event.Err))
				return
			}

		case llm.EventTextDelta:
			a.attemptUsageCommitted = false
			if event.Text != "" {
				if !emit(TextEvent(event.Text)) {
					return
				}
			}

		case llm.EventToolCall:
			// A tool call is a durable boundary. Usage from the provider attempt that
			// produced it must not be rolled back by a later provisional retry discard.
			a.markAttemptCommitted()
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
				uiEvent := ToolStartEvent(toolCallID, event.Tool.Name, toolInfo, toolArgs)
				if !emit(uiEvent) {
					return
				}
				a.stats.ToolStart()
			}

		case llm.EventToolExecStart:
			// Tool execution is also durable/replayed from the engine journal.
			a.markAttemptCommitted()
			// Skip if already seen. If toolCallID is empty, don't dedupe - treat as unique.
			if event.ToolCallID != "" {
				if _, ok := a.seenToolStarts[event.ToolCallID]; ok {
					continue
				}
				a.seenToolStarts[event.ToolCallID] = struct{}{}
			}
			uiEvent := ToolStartEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolArgs)
			if !emit(uiEvent) {
				return
			}
			a.stats.ToolStart()

		case llm.EventToolExecEnd:
			// Skip if already seen. If toolCallID is empty, don't dedupe - treat as unique.
			if event.ToolCallID != "" {
				if _, ok := a.seenToolEnds[event.ToolCallID]; ok {
					continue
				}
				a.seenToolEnds[event.ToolCallID] = struct{}{}
			}
			uiEvent := ToolEndEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolSuccess)
			if !emit(uiEvent) {
				return
			}
			a.stats.ToolEnd()
			a.resetAttemptUsage()

			// Emit image events from structured data
			for _, imagePath := range event.ToolImages {
				if !emit(ImageEvent(imagePath)) {
					return
				}
			}
			// Emit diff events from structured data
			for _, d := range event.ToolDiffs {
				if !emit(DiffEventWithOperation(d.File, d.Old, d.New, d.Line, d.Operation)) {
					return
				}
			}

		case llm.EventRetry:
			if !emit(RetryEvent(event.RetryAttempt, event.RetryMaxAttempts, event.RetryWaitSecs)) {
				return
			}

		case llm.EventAttemptDiscard:
			if a.attemptUsageCalls > 0 {
				a.stats.DiscardUsage(a.attemptInput, a.attemptOutput, a.attemptCached, a.attemptCacheWrite, a.attemptUsageCalls)
			}
			a.resetAttemptUsage()
			if !emit(AttemptDiscardEvent()) {
				return
			}

		case llm.EventUsage:
			if event.Use != nil {
				totalTokens = event.Use.OutputTokens
				usageEvent := UsageEvent(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens, event.Use.CacheWriteTokens)
				if !emit(usageEvent) {
					return
				}
				a.stats.AddUsage(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens, event.Use.CacheWriteTokens)
				if !a.attemptUsageCommitted {
					a.attemptInput += event.Use.InputTokens
					a.attemptOutput += event.Use.OutputTokens
					a.attemptCached += event.Use.CachedInputTokens
					a.attemptCacheWrite += event.Use.CacheWriteTokens
					a.attemptUsageCalls++
				}
			}

		case llm.EventPhase:
			if event.Text != "" {
				if !emit(PhaseEvent(event.Text)) {
					return
				}
			}

		case llm.EventInterjection:
			if event.Text != "" {
				if !emit(InterjectionEvent(event.Text, event.InterjectionID)) {
					return
				}
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
