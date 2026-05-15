package ui

import "encoding/json"

// StreamEventType identifies the type of stream event
type StreamEventType int

const (
	StreamEventText StreamEventType = iota
	StreamEventToolStart
	StreamEventToolEnd
	StreamEventUsage
	StreamEventPhase
	StreamEventRetry
	StreamEventDone
	StreamEventError
	StreamEventImage        // Image produced by tool
	StreamEventDiff         // Diff from edit tool
	StreamEventInterjection // User interjected a message mid-stream
	StreamEventAttemptDiscard
)

// StreamEvent represents a unified event from the LLM stream.
// Used by both ask and chat commands for consistent event handling.
type StreamEvent struct {
	Type StreamEventType

	// Text content (for StreamEventText)
	Text string

	// Interjection metadata (for StreamEventInterjection)
	InterjectionID string

	// Tool events (for StreamEventToolStart, StreamEventToolEnd)
	ToolCallID  string
	ToolName    string
	ToolInfo    string
	ToolArgs    json.RawMessage
	ToolSuccess bool

	// Usage/stats (for StreamEventUsage)
	InputTokens  int
	OutputTokens int
	CachedTokens int
	WriteTokens  int

	// Phase/retry (for StreamEventPhase, StreamEventRetry)
	Phase        string
	RetryAttempt int
	RetryMax     int
	RetryWait    float64

	// Completion (for StreamEventDone)
	Done   bool
	Tokens int // Total output tokens at completion

	// Error (for StreamEventError)
	Err error

	// Image (for StreamEventImage)
	ImagePath string

	// Diff (for StreamEventDiff)
	DiffPath      string
	DiffOld       string
	DiffNew       string
	DiffLine      int    // 1-indexed starting line number (0 = unknown)
	DiffOperation string // Optional operation hint, e.g. "create" for new files

}

// TextEvent creates a text delta event
func TextEvent(text string) StreamEvent {
	return StreamEvent{
		Type: StreamEventText,
		Text: text,
	}
}

func AttemptDiscardEvent() StreamEvent {
	return StreamEvent{Type: StreamEventAttemptDiscard}
}

// ToolStartEvent creates a tool execution start event
func ToolStartEvent(callID, name, info string, args json.RawMessage) StreamEvent {
	return StreamEvent{
		Type:       StreamEventToolStart,
		ToolCallID: callID,
		ToolName:   name,
		ToolInfo:   info,
		ToolArgs:   args,
	}
}

// ToolEndEvent creates a tool execution end event
func ToolEndEvent(callID, name, info string, success bool) StreamEvent {
	return StreamEvent{
		Type:        StreamEventToolEnd,
		ToolCallID:  callID,
		ToolName:    name,
		ToolInfo:    info,
		ToolSuccess: success,
	}
}

// UsageEvent creates a usage/token update event
func UsageEvent(input, output, cached, cacheWrite int) StreamEvent {
	return StreamEvent{
		Type:         StreamEventUsage,
		InputTokens:  input,
		OutputTokens: output,
		CachedTokens: cached,
		WriteTokens:  cacheWrite,
	}
}

// PhaseEvent creates a phase change event
func PhaseEvent(phase string) StreamEvent {
	return StreamEvent{
		Type:  StreamEventPhase,
		Phase: phase,
	}
}

// RetryEvent creates a retry notification event
func RetryEvent(attempt, max int, waitSecs float64) StreamEvent {
	return StreamEvent{
		Type:         StreamEventRetry,
		RetryAttempt: attempt,
		RetryMax:     max,
		RetryWait:    waitSecs,
	}
}

// DoneEvent creates a stream completion event
func DoneEvent(totalTokens int) StreamEvent {
	return StreamEvent{
		Type:   StreamEventDone,
		Done:   true,
		Tokens: totalTokens,
	}
}

// ErrorEvent creates an error event
func ErrorEvent(err error) StreamEvent {
	return StreamEvent{
		Type: StreamEventError,
		Err:  err,
	}
}

// ImageEvent creates an image event
func ImageEvent(path string) StreamEvent {
	return StreamEvent{
		Type:      StreamEventImage,
		ImagePath: path,
	}
}

// DiffEvent creates a diff event from edit/write tools
func DiffEvent(path, old, new string, line int) StreamEvent {
	return DiffEventWithOperation(path, old, new, line, "")
}

// DiffEventWithOperation creates a diff event with an operation hint.
func DiffEventWithOperation(path, old, new string, line int, operation string) StreamEvent {
	return StreamEvent{
		Type:          StreamEventDiff,
		DiffPath:      path,
		DiffOld:       old,
		DiffNew:       new,
		DiffLine:      line,
		DiffOperation: operation,
	}
}

// InterjectionEvent creates an interjection event (user message injected mid-stream).
func InterjectionEvent(text, interjectionID string) StreamEvent {
	return StreamEvent{
		Type:           StreamEventInterjection,
		Text:           text,
		InterjectionID: interjectionID,
	}
}
