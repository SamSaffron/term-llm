package ui

import (
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm"
)

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
	StreamEventModelSwitch  // Request model changed at a provider-turn boundary
	StreamEventAttemptDiscard
	StreamEventReasoning          // Classified, non-encrypted reasoning text/metadata
	StreamEventGenerationActivity // Hidden generation activity (for timing only)
	StreamEventFileChange         // Recorded file change metadata (file tracking)
)

// StreamEvent represents a unified event from the LLM stream.
// Used by both ask and chat commands for consistent event handling.
type StreamEvent struct {
	Type StreamEventType

	// Text content (for StreamEventText)
	Text string

	// Model switch metadata (for StreamEventModelSwitch)
	ReasoningEffort string

	// Interjection metadata (for StreamEventInterjection)
	InterjectionID string
	Message        llm.Message

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

	// File change metadata (for StreamEventFileChange). Contents are never
	// carried on events; viewers fetch diffs from the session endpoints.
	FileChange llm.FileChange

	// Reasoning (for StreamEventReasoning). Encrypted replay payloads are never
	// included here; surfaces decide display policy from the classified text.
	ReasoningKind        llm.ReasoningKind
	ReasoningText        string
	ReasoningTitle       string
	ReasoningItemID      string
	ReasoningFinal       bool
	ReasoningDisplayable bool
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

func GenerationActivityEvent() StreamEvent {
	return StreamEvent{Type: StreamEventGenerationActivity}
}

// ReasoningEvent creates a classified, non-encrypted reasoning stream event.
func ReasoningEvent(kind llm.ReasoningKind, text, title, itemID string, final bool, displayable bool) StreamEvent {
	return StreamEvent{
		Type:                 StreamEventReasoning,
		ReasoningKind:        llm.NormalizeReasoningKind(kind),
		ReasoningText:        text,
		ReasoningTitle:       title,
		ReasoningItemID:      itemID,
		ReasoningFinal:       final,
		ReasoningDisplayable: displayable,
	}
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

// RetryAttemptLabel formats retry attempt progress. A max value of 0 means the
// retry policy is time-budgeted and has no fixed attempt ceiling.
func RetryAttemptLabel(attempt, max int) string {
	if max > 0 {
		return fmt.Sprintf("%d/%d", attempt, max)
	}
	return fmt.Sprintf("attempt %d", attempt)
}

// FormatRetryStatus returns a consistent retry status string for CLI/TUI
// surfaces. precision controls the wait seconds decimal places; suffix is often
// "..." for live terminal status messages.
func FormatRetryStatus(label string, attempt, max int, waitSecs float64, precision int, suffix string) string {
	if label == "" {
		label = "Retrying"
	}
	if precision < 0 {
		precision = 0
	}
	wait := fmt.Sprintf("%.*f", precision, waitSecs)
	return fmt.Sprintf("%s (%s), waiting %ss%s", label, RetryAttemptLabel(attempt, max), wait, suffix)
}

// RetryStatus formats this event as retry status text.
func (e StreamEvent) RetryStatus(label string, precision int, suffix string) string {
	return FormatRetryStatus(label, e.RetryAttempt, e.RetryMax, e.RetryWait, precision, suffix)
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

// FileChangeEvent creates a file-change metadata event (file tracking).
func FileChangeEvent(fc llm.FileChange) StreamEvent {
	return StreamEvent{
		Type:       StreamEventFileChange,
		FileChange: fc,
	}
}

// ModelSwitchEvent creates an event for an in-run request model change.
func ModelSwitchEvent(model, reasoningEffort string) StreamEvent {
	return StreamEvent{
		Type:            StreamEventModelSwitch,
		Text:            model,
		ReasoningEffort: reasoningEffort,
	}
}

// InterjectionEvent creates an interjection event (user message injected mid-stream).
func InterjectionEvent(text, interjectionID string) StreamEvent {
	return InterjectionEventWithMessage(text, interjectionID, llm.Message{})
}

// InterjectionEventWithMessage creates an interjection event with structured content.
func InterjectionEventWithMessage(text, interjectionID string, msg llm.Message) StreamEvent {
	return StreamEvent{
		Type:           StreamEventInterjection,
		Text:           text,
		InterjectionID: interjectionID,
		Message:        msg,
	}
}
