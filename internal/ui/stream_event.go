package ui

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
)

// StreamEvent represents a unified event from the LLM stream.
// Used by both ask and chat commands for consistent event handling.
type StreamEvent struct {
	Type StreamEventType

	// Text content (for StreamEventText)
	Text string

	// Tool events (for StreamEventToolStart, StreamEventToolEnd)
	ToolCallID  string
	ToolName    string
	ToolInfo    string
	ToolSuccess bool

	// Usage/stats (for StreamEventUsage)
	InputTokens  int
	OutputTokens int
	CachedTokens int

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
}

// TextEvent creates a text delta event
func TextEvent(text string) StreamEvent {
	return StreamEvent{
		Type: StreamEventText,
		Text: text,
	}
}

// ToolStartEvent creates a tool execution start event
func ToolStartEvent(callID, name, info string) StreamEvent {
	return StreamEvent{
		Type:       StreamEventToolStart,
		ToolCallID: callID,
		ToolName:   name,
		ToolInfo:   info,
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
func UsageEvent(input, output, cached int) StreamEvent {
	return StreamEvent{
		Type:         StreamEventUsage,
		InputTokens:  input,
		OutputTokens: output,
		CachedTokens: cached,
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
